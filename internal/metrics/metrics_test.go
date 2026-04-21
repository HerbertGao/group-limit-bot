package metrics

import (
	"testing"
	"time"
)

func TestRegistry_PerGroupErrorIsolation(t *testing.T) {
	r := NewRegistry()
	const g1, g2 int64 = -1001, -1002
	base := time.Unix(1_000, 0)

	// Record 6 errors for each group. Each group's ring should cap at 5.
	for i := 0; i < 6; i++ {
		r.RecordError(ErrorRecord{At: base.Add(time.Duration(i) * time.Second), Op: "getChatMember", GroupChatID: g1, Err: "G1"})
		r.RecordError(ErrorRecord{At: base.Add(time.Duration(i) * time.Second), Op: "getChatMember", GroupChatID: g2, Err: "G2"})
	}

	g1Errs := r.RecentErrorsForGroup(g1)
	g2Errs := r.RecentErrorsForGroup(g2)

	if len(g1Errs) != 5 {
		t.Errorf("g1 expected 5 entries, got %d", len(g1Errs))
	}
	if len(g2Errs) != 5 {
		t.Errorf("g2 expected 5 entries, got %d", len(g2Errs))
	}
	for _, e := range g1Errs {
		if e.GroupChatID != g1 {
			t.Errorf("g1 ring leaked foreign GroupChatID=%d", e.GroupChatID)
		}
		if e.Err != "G1" {
			t.Errorf("g1 ring leaked foreign Err=%q", e.Err)
		}
	}
	for _, e := range g2Errs {
		if e.GroupChatID != g2 {
			t.Errorf("g2 ring leaked foreign GroupChatID=%d", e.GroupChatID)
		}
		if e.Err != "G2" {
			t.Errorf("g2 ring leaked foreign Err=%q", e.Err)
		}
	}
}
