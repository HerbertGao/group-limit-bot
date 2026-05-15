package gating

import (
	"context"
	"log/slog"
	"sync"

	"github.com/herbertgao/group-limit-bot/internal/store"
)

// allowlistLister is the subset of the store the GroupAllowlist depends on.
type allowlistLister interface {
	ListAllowedBots(ctx context.Context, groupID int64) ([]store.AllowedBot, error)
}

// GroupAllowlist answers "is bot B allowed in group G" against the per-group
// `group_bot_allowlist` table, with a lazily-populated in-memory cache. The
// cache for a group must be invalidated (Invalidate) whenever /allowbot or
// /disallowbot mutates that group's allowlist, and on /unbind.
//
// A nil *GroupAllowlist is valid and reports every bot as not-allowed; this
// keeps the per-group layer optional for tests and the global-only path.
type GroupAllowlist struct {
	store allowlistLister
	log   *slog.Logger

	mu    sync.Mutex
	cache map[int64]map[int64]struct{}
}

func NewGroupAllowlist(st allowlistLister, log *slog.Logger) *GroupAllowlist {
	return &GroupAllowlist{
		store: st,
		log:   log,
		cache: make(map[int64]map[int64]struct{}),
	}
}

// Allowed reports whether botID is in groupID's per-group allowlist.
// On a store error the group is treated as having an empty allowlist (the bot
// is not allowed) — an ad bot must never be allowed through by a read failure.
func (a *GroupAllowlist) Allowed(ctx context.Context, groupID, botID int64) bool {
	if a == nil {
		return false
	}
	set := a.groupSet(ctx, groupID)
	_, ok := set[botID]
	return ok
}

// groupSet returns the cached bot-id set for groupID, loading it from the store
// on a cache miss.
func (a *GroupAllowlist) groupSet(ctx context.Context, groupID int64) map[int64]struct{} {
	a.mu.Lock()
	if set, ok := a.cache[groupID]; ok {
		a.mu.Unlock()
		return set
	}
	a.mu.Unlock()

	set := make(map[int64]struct{})
	rows, err := a.store.ListAllowedBots(ctx, groupID)
	if err != nil {
		if a.log != nil {
			a.log.Warn("group allowlist load failed",
				slog.Int64("group_id", groupID),
				slog.String("error", err.Error()),
			)
		}
		// Do not cache a failed load — retry on the next message.
		return set
	}
	for _, r := range rows {
		set[r.BotUserID] = struct{}{}
	}
	a.mu.Lock()
	a.cache[groupID] = set
	a.mu.Unlock()
	return set
}

// Invalidate drops the cached set for groupID so the next lookup reloads it.
func (a *GroupAllowlist) Invalidate(groupID int64) {
	if a == nil {
		return
	}
	a.mu.Lock()
	delete(a.cache, groupID)
	a.mu.Unlock()
}
