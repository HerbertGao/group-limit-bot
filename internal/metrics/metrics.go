package metrics

import (
	"sort"
	"sync"
	"time"
)

// DeleteRecord represents a single delete event.
type DeleteRecord struct {
	GroupID int64
	At      time.Time
}

// ErrorRecord captures a recent getChatMember (or other) failure for /status display.
// ChatID is the API target (e.g. the channel for getChatMember); GroupChatID
// identifies which bound group the error affected, for per-group /status filtering.
// RetryAfter is populated only when the underlying error is a rate-limit (429);
// it is zero-valued for all other failures.
type ErrorRecord struct {
	At          time.Time
	Op          string
	ChatID      int64
	GroupChatID int64
	UserID      int64
	Err         string
	RetryAfter  time.Duration
}

// Registry holds in-memory metrics: per-group 1h delete window + per-group small error ring.
type Registry struct {
	mu            sync.Mutex
	deletesByGrp  map[int64][]time.Time
	deleteWindow  time.Duration
	errorsByGrp   map[int64][]ErrorRecord
	errorCapacity int
}

func NewRegistry() *Registry {
	return &Registry{
		deletesByGrp:  make(map[int64][]time.Time),
		deleteWindow:  time.Hour,
		errorsByGrp:   make(map[int64][]ErrorRecord),
		errorCapacity: 5,
	}
}

// RecordDelete records a successful delete at time t.
func (r *Registry) RecordDelete(groupID int64, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletesByGrp[groupID] = append(r.deletesByGrp[groupID], t)
	r.pruneLocked(groupID, t)
}

// CountRecentDeletes returns the delete count in the last [window] for groupID.
func (r *Registry) CountRecentDeletes(groupID int64, now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(groupID, now)
	return len(r.deletesByGrp[groupID])
}

func (r *Registry) pruneLocked(groupID int64, now time.Time) {
	cutoff := now.Add(-r.deleteWindow)
	list := r.deletesByGrp[groupID]
	idx := 0
	for idx < len(list) && list[idx].Before(cutoff) {
		idx++
	}
	if idx > 0 {
		r.deletesByGrp[groupID] = list[idx:]
	}
}

// RecordError appends an error record to the ring for its GroupChatID,
// dropping the oldest when that group's ring is full.
func (r *Registry) RecordError(rec ErrorRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := append(r.errorsByGrp[rec.GroupChatID], rec)
	if len(list) > r.errorCapacity {
		list = list[len(list)-r.errorCapacity:]
	}
	r.errorsByGrp[rec.GroupChatID] = list
}

// RecentErrors returns all groups' recent errors composed into one slice,
// sorted by time ascending (oldest first). Retained for backward compat.
func (r *Registry) RecentErrors() []ErrorRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ErrorRecord
	for _, list := range r.errorsByGrp {
		out = append(out, list...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// RecentErrorsForGroup returns a copy of the per-group ring for groupID
// (oldest first).
func (r *Registry) RecentErrorsForGroup(groupID int64) []ErrorRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.errorsByGrp[groupID]
	out := make([]ErrorRecord, len(list))
	copy(out, list)
	return out
}
