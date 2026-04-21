package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestBindings_UpsertCreatesThenUpdates(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	created, channelChanged, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: -100, ChannelChatID: -200, BoundByUserID: 1, BoundAt: time.Unix(100, 0),
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !created {
		t.Error("first upsert should be created=true")
	}
	if channelChanged {
		t.Error("first upsert should not report channel changed")
	}

	created, channelChanged, err = s.UpsertBinding(ctx, Binding{
		GroupChatID: -100, ChannelChatID: -300, BoundByUserID: 2, BoundAt: time.Unix(200, 0),
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if created {
		t.Error("second upsert should be created=false")
	}
	if !channelChanged {
		t.Error("second upsert changed channel_chat_id, expected channelChanged=true")
	}

	got, err := s.GetBinding(ctx, -100)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected binding")
	}
	if got.ChannelChatID != -300 || got.BoundByUserID != 2 {
		t.Errorf("update did not persist: %+v", got)
	}
}

func TestUpsertBinding_ChannelChangeCascadesVerified(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if _, _, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: -100, ChannelChatID: -200, BoundByUserID: 1, BoundAt: time.Unix(100, 0),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertVerified(ctx, -100, -200, 5, time.Unix(99999, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVerified(ctx, -100, -200, 6, time.Unix(99999, 0)); err != nil {
		t.Fatal(err)
	}
	// Unrelated group must survive.
	if _, _, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: -101, ChannelChatID: -201, BoundByUserID: 1, BoundAt: time.Unix(100, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVerified(ctx, -101, -201, 7, time.Unix(99999, 0)); err != nil {
		t.Fatal(err)
	}

	// Re-bind same channel, different admin/time: verified rows must be preserved.
	_, channelChanged, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: -100, ChannelChatID: -200, BoundByUserID: 2, BoundAt: time.Unix(150, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if channelChanged {
		t.Error("channel unchanged, channelChanged should be false")
	}
	n, err := s.CountVerifiedInChannel(ctx, -100, -200, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("verified rows should survive no-channel-change upsert, got %d", n)
	}

	// Now switch channel: verified rows for that group must be wiped, others preserved.
	_, channelChanged, err = s.UpsertBinding(ctx, Binding{
		GroupChatID: -100, ChannelChatID: -300, BoundByUserID: 2, BoundAt: time.Unix(200, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !channelChanged {
		t.Error("channel changed, channelChanged should be true")
	}

	n, err = s.CountVerifiedInChannel(ctx, -100, -300, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("verified rows for -100 should be wiped, got %d", n)
	}
	n, err = s.CountVerifiedInChannel(ctx, -101, -201, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("verified rows for -101 should be unaffected, got %d", n)
	}
}

func TestBindings_DeleteCascadesVerified(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	_, _, err := s.UpsertBinding(ctx, Binding{GroupChatID: -100, ChannelChatID: -200, BoundAt: time.Unix(1, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVerified(ctx, -100, -200, 5, time.Unix(99999, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVerified(ctx, -100, -200, 6, time.Unix(99999, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVerified(ctx, -101, -201, 7, time.Unix(99999, 0)); err != nil {
		t.Fatal(err)
	}

	removed, err := s.DeleteBinding(ctx, -100)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Error("expected removed=true")
	}

	n, err := s.CountVerifiedInChannel(ctx, -100, -200, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("verified for -100 should be cleared, got %d", n)
	}
	n, err = s.CountVerifiedInChannel(ctx, -101, -201, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("verified for -101 should remain, got %d", n)
	}
}

func TestBindings_DeleteMissing(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	removed, err := s.DeleteBinding(ctx, -999)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Error("expected removed=false when no row")
	}
}

// Invariant: when DeleteBinding is called for a group that has no binding row,
// it must be a true no-op — in particular it must NOT delete verified_members
// rows that might exist for that group_chat_id from a prior lifecycle.
func TestDeleteBinding_NoOpWhenNotBound(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	// Seed verified_members for group -2 via the IfBound API, which requires a
	// binding to exist. Then remove the binding row directly (bypassing
	// DeleteBinding) to simulate stale verified_members without a binding.
	if _, _, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: -2, ChannelChatID: -20, BoundByUserID: 1, BoundAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("upsert binding: %v", err)
	}
	applied, err := s.UpsertVerifiedIfBound(ctx, -2, -20, 7, 1, time.Unix(99999, 0))
	if err != nil {
		t.Fatalf("upsert verified: %v", err)
	}
	if !applied {
		t.Fatal("expected verified write to apply with binding present")
	}
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM bindings WHERE group_chat_id = ?`, int64(-2)); err != nil {
		t.Fatalf("raw delete binding: %v", err)
	}

	// Now DeleteBinding(-2) must report removed=false AND leave the orphaned
	// verified_members row intact (since no binding row was actually removed).
	removed, err := s.DeleteBinding(ctx, -2)
	if err != nil {
		t.Fatalf("DeleteBinding: %v", err)
	}
	if removed {
		t.Error("expected removed=false when binding missing")
	}
	n, err := s.CountVerifiedInChannel(ctx, -2, -20, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("verified_members for -2 must be preserved when DeleteBinding is a no-op, got %d", n)
	}
}

func TestVerified_GetMissThenHitThenExpire(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Unix(1_000, 0)

	_, ok, err := s.GetVerified(ctx, -1, -10, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("miss should be false")
	}

	if err := s.UpsertVerified(ctx, -1, -10, 2, now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}

	_, ok, err = s.GetVerified(ctx, -1, -10, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("hit should be true")
	}

	_, ok, err = s.GetVerified(ctx, -1, -10, 2, now.Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expired row should be miss")
	}
}

func TestVerified_DeleteExpired(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Unix(1_000, 0)
	if err := s.UpsertVerified(ctx, -1, -10, 1, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVerified(ctx, -1, -10, 2, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteExpiredVerified(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 expired deleted, got %d", n)
	}

	valid, err := s.LoadAllValidVerified(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 1 || valid[0].UserID != 2 {
		t.Errorf("unexpected valid set: %+v", valid)
	}
}

func TestOpen_FileBackedEnablesWALAndBusyTimeout(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "bot.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	var journalMode string
	if err := s.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := s.DB().QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}
}

func TestOpen_InMemoryForcesSingleConn(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := s.DB().Stats().MaxOpenConnections; got != 1 {
		t.Errorf(":memory: must pin to 1 open connection, got %d", got)
	}
}

func TestVerified_ChannelScoped(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Unix(1_000, 0)

	if err := s.UpsertVerified(ctx, -1, -10, 42, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVerified(ctx, -1, -20, 42, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := s.GetVerified(ctx, -1, -10, 42, now); err != nil || !ok {
		t.Errorf("(G,C1,U) should hit: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.GetVerified(ctx, -1, -20, 42, now); err != nil || !ok {
		t.Errorf("(G,C2,U) should hit: ok=%v err=%v", ok, err)
	}
	// Cross-channel lookup must miss.
	if _, ok, err := s.GetVerified(ctx, -1, -30, 42, now); err != nil || ok {
		t.Errorf("(G,C3,U) must miss: ok=%v err=%v", ok, err)
	}

	valid, err := s.LoadAllValidVerified(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) != 2 {
		t.Fatalf("expected 2 distinct rows, got %d: %+v", len(valid), valid)
	}
	seen := map[int64]bool{}
	for _, v := range valid {
		if v.GroupChatID != -1 || v.UserID != 42 {
			t.Errorf("unexpected row: %+v", v)
		}
		seen[v.ChannelChatID] = true
	}
	if !seen[-10] || !seen[-20] {
		t.Errorf("expected both channels represented, got %v", seen)
	}
}

func TestCountVerifiedInChannel_IgnoresOtherChannels(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Unix(1_000, 0)

	const (
		g  int64 = -500
		c1 int64 = -600
		c2 int64 = -700
		u  int64 = 42
	)
	if err := s.UpsertVerified(ctx, g, c1, u, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVerified(ctx, g, c2, u, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	n, err := s.CountVerifiedInChannel(ctx, g, c1, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("c1 scope: got %d, want 1", n)
	}
	n, err = s.CountVerifiedInChannel(ctx, g, c2, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("c2 scope: got %d, want 1", n)
	}
}

func TestUpsertVerifiedIfBound_RequiresMatchingBinding(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Unix(1_000, 0)

	// No binding yet: must not write.
	applied, err := s.UpsertVerifiedIfBound(ctx, -1, -10, 5, 1, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if applied {
		t.Error("expected applied=false with no binding")
	}
	if _, ok, err := s.GetVerified(ctx, -1, -10, 5, now); err != nil || ok {
		t.Errorf("row must not exist when binding missing: ok=%v err=%v", ok, err)
	}

	// Create binding, then the write must apply.
	if _, _, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: -1, ChannelChatID: -10, BoundByUserID: 1, BoundAt: time.Unix(100, 0),
	}); err != nil {
		t.Fatal(err)
	}
	b, err := s.GetBinding(ctx, -1)
	if err != nil || b == nil {
		t.Fatalf("get binding: %v", err)
	}
	applied, err = s.UpsertVerifiedIfBound(ctx, -1, -10, 5, b.Epoch, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !applied {
		t.Error("expected applied=true with matching binding")
	}
	if _, ok, err := s.GetVerified(ctx, -1, -10, 5, now); err != nil || !ok {
		t.Errorf("row must exist after applied write: ok=%v err=%v", ok, err)
	}

	// Delete binding; verified row is cascade-deleted. Subsequent write must not apply.
	removed, err := s.DeleteBinding(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("expected binding removed")
	}
	applied, err = s.UpsertVerifiedIfBound(ctx, -1, -10, 5, b.Epoch, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if applied {
		t.Error("expected applied=false after binding deleted")
	}
	if _, ok, err := s.GetVerified(ctx, -1, -10, 5, now); err != nil || ok {
		t.Errorf("row must not exist after binding deleted: ok=%v err=%v", ok, err)
	}

	// Binding to a different channel must not allow writes for the old channel.
	if _, _, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: -1, ChannelChatID: -20, BoundByUserID: 1, BoundAt: time.Unix(100, 0),
	}); err != nil {
		t.Fatal(err)
	}
	b2, err := s.GetBinding(ctx, -1)
	if err != nil || b2 == nil {
		t.Fatalf("get binding: %v", err)
	}
	applied, err = s.UpsertVerifiedIfBound(ctx, -1, -10, 5, b2.Epoch, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Error("expected applied=false when binding channel differs")
	}
}

// TestUpsertVerifiedIfBound_SingleStatement verifies that the binding check
// and the verified_members write are performed atomically in a single SQL
// statement: seeding a binding, writing, deleting the binding, and attempting
// another write must all behave correctly without any explicit transaction.
func TestUpsertVerifiedIfBound_SingleStatement(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Unix(1_000, 0)
	expiresAt := now.Add(time.Hour)

	const G, C, U = int64(-100), int64(-1000), int64(7)

	if _, _, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: G, ChannelChatID: C, BoundByUserID: 1, BoundAt: time.Unix(100, 0),
	}); err != nil {
		t.Fatal(err)
	}
	b, err := s.GetBinding(ctx, G)
	if err != nil || b == nil {
		t.Fatalf("get binding: %v", err)
	}

	applied, err := s.UpsertVerifiedIfBound(ctx, G, C, U, b.Epoch, expiresAt)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !applied {
		t.Fatal("expected applied=true with matching binding")
	}
	if _, ok, err := s.GetVerified(ctx, G, C, U, now); err != nil || !ok {
		t.Fatalf("row must exist after applied write: ok=%v err=%v", ok, err)
	}

	removed, err := s.DeleteBinding(ctx, G)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("expected binding removed")
	}

	applied, err = s.UpsertVerifiedIfBound(ctx, G, C, U, b.Epoch, expiresAt)
	if err != nil {
		t.Fatalf("upsert after delete: %v", err)
	}
	if applied {
		t.Error("expected applied=false after binding deleted")
	}
	if _, ok, err := s.GetVerified(ctx, G, C, U, now); err != nil || ok {
		t.Errorf("row must not exist after binding deleted: ok=%v err=%v", ok, err)
	}
}

// TestUpsertVerifiedIfBound_EpochMismatchBlocksStaleWrite covers the
// unbind+rebind-same-channel race: gate captured binding at epoch N, but by the
// time the Set write runs, /unbind + /bind (same channel) has bumped the epoch.
// The stale write must not land.
func TestUpsertVerifiedIfBound_EpochMismatchBlocksStaleWrite(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Unix(1_000, 0)
	expiresAt := now.Add(time.Hour)

	const G, C, Cother, U = int64(-500), int64(-600), int64(-700), int64(42)

	// Initial bind G -> C: epoch=1.
	if _, _, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: G, ChannelChatID: C, BoundByUserID: 1, BoundAt: time.Unix(100, 0),
	}); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBinding(ctx, G)
	if err != nil || b1 == nil {
		t.Fatalf("get binding: %v", err)
	}
	epochBefore := b1.Epoch

	// Rebind to a different channel then back to C; epoch is now >= 3.
	if _, _, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: G, ChannelChatID: Cother, BoundByUserID: 1, BoundAt: time.Unix(200, 0),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: G, ChannelChatID: C, BoundByUserID: 1, BoundAt: time.Unix(300, 0),
	}); err != nil {
		t.Fatal(err)
	}
	bNew, err := s.GetBinding(ctx, G)
	if err != nil || bNew == nil {
		t.Fatalf("get binding: %v", err)
	}
	if bNew.Epoch == epochBefore {
		t.Fatalf("epoch did not advance after rebind (before=%d, now=%d)", epochBefore, bNew.Epoch)
	}

	// Stale write under the old epoch must NOT land.
	applied, err := s.UpsertVerifiedIfBound(ctx, G, C, U, epochBefore, expiresAt)
	if err != nil {
		t.Fatalf("stale upsert: %v", err)
	}
	if applied {
		t.Error("expected applied=false with stale epoch")
	}
	if _, ok, err := s.GetVerified(ctx, G, C, U, now); err != nil || ok {
		t.Errorf("row must not exist after stale-epoch write: ok=%v err=%v", ok, err)
	}

	// Fresh write with the current epoch succeeds.
	applied, err = s.UpsertVerifiedIfBound(ctx, G, C, U, bNew.Epoch, expiresAt)
	if err != nil {
		t.Fatalf("fresh upsert: %v", err)
	}
	if !applied {
		t.Error("expected applied=true with current epoch")
	}
	if _, ok, err := s.GetVerified(ctx, G, C, U, now); err != nil || !ok {
		t.Errorf("row must exist after fresh-epoch write: ok=%v err=%v", ok, err)
	}
}

// TestUpsertBinding_EpochMonotonicAcrossUnbindRebind covers the critical
// race: unbind + rebind to the same channel must never reuse an epoch a
// previous Set-in-flight already captured.
func TestUpsertBinding_EpochMonotonicAcrossUnbindRebind(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	const G, C, Cother, U = int64(-800), int64(-900), int64(-901), int64(77)

	// First bind -> epoch=1
	if _, _, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: G, ChannelChatID: C, BoundByUserID: 1, BoundAt: time.Unix(100, 0),
	}); err != nil {
		t.Fatal(err)
	}
	b1, err := s.GetBinding(ctx, G)
	if err != nil || b1 == nil {
		t.Fatalf("get: %v", err)
	}
	if b1.Epoch != 1 {
		t.Fatalf("first upsert expected epoch=1, got %d", b1.Epoch)
	}

	// Re-upsert same group, different channel -> epoch=2
	if _, _, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: G, ChannelChatID: Cother, BoundByUserID: 1, BoundAt: time.Unix(200, 0),
	}); err != nil {
		t.Fatal(err)
	}
	b2, err := s.GetBinding(ctx, G)
	if err != nil || b2 == nil {
		t.Fatalf("get: %v", err)
	}
	if b2.Epoch != 2 {
		t.Fatalf("second upsert expected epoch=2, got %d", b2.Epoch)
	}

	// DeleteBinding must NOT reset the counter.
	removed, err := s.DeleteBinding(ctx, G)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("expected removed=true")
	}

	// Re-upsert same group same channel as first -> epoch MUST be 3 (strictly > 2).
	if _, _, err := s.UpsertBinding(ctx, Binding{
		GroupChatID: G, ChannelChatID: C, BoundByUserID: 1, BoundAt: time.Unix(300, 0),
	}); err != nil {
		t.Fatal(err)
	}
	b3, err := s.GetBinding(ctx, G)
	if err != nil || b3 == nil {
		t.Fatalf("get: %v", err)
	}
	if b3.Epoch != 3 {
		t.Fatalf("reinsert after delete expected epoch=3, got %d", b3.Epoch)
	}

	// In-flight Set captured at epoch=1 must fail under the new binding.
	applied, err := s.UpsertVerifiedIfBound(ctx, G, C, U, 1, time.Unix(99999, 0))
	if err != nil {
		t.Fatalf("stale upsert: %v", err)
	}
	if applied {
		t.Error("expected applied=false for stale epoch=1 after rebind (epoch=3)")
	}
}

func TestBindings_List(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	_, _, _ = s.UpsertBinding(ctx, Binding{GroupChatID: -1, ChannelChatID: -10, BoundAt: time.Unix(1, 0)})
	_, _, _ = s.UpsertBinding(ctx, Binding{GroupChatID: -2, ChannelChatID: -20, BoundAt: time.Unix(2, 0)})
	got, err := s.ListBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2, got %d", len(got))
	}
}
