package gating

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/herbertgao/group-limit-bot/internal/store"
)

type memberKey struct {
	GroupID   int64
	ChannelID int64
	UserID    int64
}

// MemberCache is an in-memory L1 cache backed by SQLite as L2.
// Only positive ("verified, still in channel") results are cached; misses are not cached,
// so that users who follow the channel gain access without waiting for a TTL to expire.
type MemberCache struct {
	mu sync.RWMutex
	// mem holds the hot in-memory entries. generations tracks a per-group counter
	// bumped by DropGroup so that Set can detect a racing /unbind that cascaded
	// its SQL row away between UpsertVerifiedIfBound and the mem write.
	mem         map[memberKey]time.Time
	generations map[int64]uint64
	store       *store.Store
	ttl         time.Duration
	maxEntries  int
}

func NewMemberCache(st *store.Store, ttl time.Duration) *MemberCache {
	return &MemberCache{
		mem:         make(map[memberKey]time.Time),
		generations: make(map[int64]uint64),
		store:       st,
		ttl:         ttl,
		maxEntries:  10000,
	}
}

// Prime loads all still-valid rows from SQLite into memory. Call once at startup.
func (c *MemberCache) Prime(ctx context.Context, now time.Time) error {
	rows, err := c.store.LoadAllValidVerified(ctx, now)
	if err != nil {
		return err
	}
	if len(rows) > c.maxEntries {
		sort.Slice(rows, func(i, j int) bool {
			return rows[i].ExpiresAt.After(rows[j].ExpiresAt)
		})
		rows = rows[:c.maxEntries]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range rows {
		c.mem[memberKey{v.GroupChatID, v.ChannelChatID, v.UserID}] = v.ExpiresAt
	}
	return nil
}

// Get returns true when (groupID, channelID, userID) has a non-expired entry.
// Falls back to SQLite on miss to survive process restarts before a full prime.
func (c *MemberCache) Get(ctx context.Context, groupID, channelID, userID int64, now time.Time) (bool, error) {
	k := memberKey{groupID, channelID, userID}

	c.mu.RLock()
	exp, ok := c.mem[k]
	c.mu.RUnlock()
	if ok {
		if exp.After(now) {
			return true, nil
		}
		c.mu.Lock()
		if cur, stillThere := c.mem[k]; stillThere && !cur.After(now) {
			delete(c.mem, k)
		}
		c.mu.Unlock()
	}

	expStore, hit, err := c.store.GetVerified(ctx, groupID, channelID, userID, now)
	if err != nil {
		return false, err
	}
	if hit {
		c.mu.Lock()
		if _, exists := c.mem[k]; !exists && len(c.mem) >= c.maxEntries {
			evictOldestLocked(c.mem, c.maxEntries/10)
		}
		c.mem[k] = expStore
		c.mu.Unlock()
	}
	return hit, nil
}

// Set writes SQLite first, then the in-memory map, so a failed store write does
// not leave memory claiming a verification that isn't persisted. The write is
// guarded by a same-transaction check that a matching binding still exists, to
// prevent flight-in-flight approvals from reinstating cache entries after
// /unbind. Additionally, a per-group generation counter (bumped by DropGroup)
// is sampled before the SQL call and re-checked under mu.Lock so that a racing
// /unbind — which cascaded our just-written row away — cannot leave a stale
// mem entry with no SQL counterpart.
func (c *MemberCache) Set(ctx context.Context, groupID, channelID, userID int64, bindingEpoch int64, now time.Time) error {
	genBefore := c.groupGeneration(groupID)
	expires := now.Add(c.ttl)
	applied, err := c.store.UpsertVerifiedIfBound(ctx, groupID, channelID, userID, bindingEpoch, expires)
	if err != nil {
		return err
	}
	if !applied {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generations[groupID] != genBefore {
		// DropGroup raced; the cascade also wiped our SQL row. Don't poison mem.
		return nil
	}
	k := memberKey{groupID, channelID, userID}
	if _, exists := c.mem[k]; !exists && len(c.mem) >= c.maxEntries {
		evictOldestLocked(c.mem, c.maxEntries/10)
	}
	c.mem[k] = expires
	return nil
}

// evictOldestLocked removes up to n entries with the earliest expiration.
// Caller must hold c.mu for write.
func evictOldestLocked(m map[memberKey]time.Time, n int) {
	if n <= 0 || len(m) == 0 {
		return
	}
	type entry struct {
		k memberKey
		t time.Time
	}
	entries := make([]entry, 0, len(m))
	for k, t := range m {
		entries = append(entries, entry{k, t})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].t.Before(entries[j].t)
	})
	limit := n
	if limit > len(entries) {
		limit = len(entries)
	}
	for i := 0; i < limit; i++ {
		delete(m, entries[i].k)
	}
}

// groupGeneration returns the current generation counter for a group; DropGroup
// bumps it so Set can detect a race.
func (c *MemberCache) groupGeneration(groupID int64) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.generations[groupID]
}

// DropGroup removes all in-memory entries for a group (e.g. on unbind) and
// bumps the per-group generation so any in-flight Set skips its mem write.
func (c *MemberCache) DropGroup(groupID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.mem {
		if k.GroupID == groupID {
			delete(c.mem, k)
		}
	}
	c.generations[groupID]++
}

// Prune removes expired in-memory entries. Call periodically alongside store.DeleteExpiredVerified.
func (c *MemberCache) Prune(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, exp := range c.mem {
		if !exp.After(now) {
			delete(c.mem, k)
		}
	}
}

// -------- Media group deduplication --------

type dedupEntry struct {
	decision Decision
	reason   Reason
	expires  time.Time
}

// MediaGroupDedup caches the first decision for a media_group_id so subsequent
// messages in the same album reuse the same outcome without hitting the API.
// It also provides single-flight semantics: the first goroutine for an id
// becomes the leader and computes the decision; other goroutines wait until
// the leader publishes, then reuse that decision.
type MediaGroupDedup struct {
	mu      sync.Mutex
	entries map[string]dedupEntry
	ttl     time.Duration
	// pending tracks in-flight decisions: first caller computes, others wait.
	pending map[string]chan struct{}
}

func NewMediaGroupDedup(ttl time.Duration) *MediaGroupDedup {
	return &MediaGroupDedup{
		entries: make(map[string]dedupEntry),
		pending: make(map[string]chan struct{}),
		ttl:     ttl,
	}
}

// Acquire returns:
//
//	hit=true  -> an entry already exists for id; decision and reason are valid.
//	hit=false, leader=true -> caller is the first goroutine for id; MUST compute
//	    a decision and then call Publish(id, decision, reason, now). If it cannot
//	    produce a decision, it MUST call Abort(id) so waiters can retry.
//	hit=false, leader=false -> another goroutine is already computing; caller
//	    waited for it and Acquire has returned so the caller can re-call Acquire
//	    (which will typically see the published entry as a hit).
//
// If ctx is canceled while waiting, returns ctx.Err().
func (d *MediaGroupDedup) Acquire(ctx context.Context, id string, now time.Time) (decision Decision, reason Reason, hit, leader bool, err error) {
	if id == "" {
		return 0, "", false, true, nil
	}
	for {
		d.mu.Lock()
		if e, ok := d.entries[id]; ok && e.expires.After(now) {
			d.mu.Unlock()
			return e.decision, e.reason, true, false, nil
		} else if ok {
			delete(d.entries, id)
		}
		if ch, inflight := d.pending[id]; inflight {
			d.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return 0, "", false, false, ctx.Err()
			}
		}
		ch := make(chan struct{})
		d.pending[id] = ch
		d.mu.Unlock()
		return 0, "", false, true, nil
	}
}

// Publish records the leader's decision and wakes any waiters.
func (d *MediaGroupDedup) Publish(id string, dec Decision, rsn Reason, now time.Time) {
	if id == "" {
		return
	}
	d.mu.Lock()
	d.entries[id] = dedupEntry{decision: dec, reason: rsn, expires: now.Add(d.ttl)}
	ch := d.pending[id]
	delete(d.pending, id)
	needCompact := len(d.entries) > 1024
	d.mu.Unlock()
	if ch != nil {
		close(ch)
	}
	if needCompact {
		d.mu.Lock()
		for k, e := range d.entries {
			if !e.expires.After(now) {
				delete(d.entries, k)
			}
		}
		d.mu.Unlock()
	}
}

// Abort cleans up pending state without publishing a decision. Waiters resume
// and will observe no entry (one of them may then become the new leader).
func (d *MediaGroupDedup) Abort(id string) {
	if id == "" {
		return
	}
	d.mu.Lock()
	ch := d.pending[id]
	delete(d.pending, id)
	d.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}
