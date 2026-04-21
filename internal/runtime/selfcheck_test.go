package runtime

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/herbertgao/group-limit-bot/internal/store"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

// TestSelfCheck_CooldownResetsOnRecovery verifies that clearWarned drops the
// lastWarned entry for a group so that a subsequent degradation can fire an
// immediate alert rather than being suppressed by stale cooldown state.
func TestSelfCheck_CooldownResetsOnRecovery(t *testing.T) {
	sc := &SelfCheck{
		lastWarned:   make(map[int64]time.Time),
		warnCooldown: 24 * time.Hour,
	}

	const gid = int64(42)

	sc.markWarned(gid)
	if sc.canWarn(gid) {
		t.Fatal("expected canWarn=false immediately after markWarned")
	}

	sc.clearWarned(gid)
	if !sc.canWarn(gid) {
		t.Fatal("expected canWarn=true after clearWarned")
	}
}

// TestSelfCheck_TransientErrorPreservesCooldown asserts that on a transient
// error (*RateLimitError / deadline / cancel), checkBinding classifies the
// outcome as not-degraded-but-not-positive, so the tick caller must leave
// cooldown state untouched.
func TestSelfCheck_TransientErrorPreservesCooldown(t *testing.T) {
	const G, C = int64(-1), int64(-10)
	const botID = int64(999)

	mock := telegram.NewMockClient(telegram.User{ID: botID, IsBot: true})
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusUnknown, &telegram.RateLimitError{ChatID: chatID, RetryAfter: 5 * time.Second}
	}
	mock.GetChatMemberCanDeleteFn = func(ctx context.Context, chatID, userID int64) (bool, error) {
		return false, &telegram.RateLimitError{ChatID: chatID, RetryAfter: 5 * time.Second}
	}

	sc := &SelfCheck{
		tg:           mock,
		log:          slog.New(slog.NewJSONHandler(io.Discard, nil)),
		lastWarned:   make(map[int64]time.Time),
		warnCooldown: time.Hour,
	}

	// Seed cooldown.
	sc.markWarned(G)
	if sc.canWarn(G) {
		t.Fatal("expected cooldown active after markWarned")
	}

	b := store.Binding{GroupChatID: G, ChannelChatID: C}
	reasons, channelOK, groupOK := sc.checkBinding(context.Background(), b, botID)
	if len(reasons) != 0 {
		t.Errorf("transient errors must not produce degraded reasons, got %v", reasons)
	}
	if channelOK || groupOK {
		t.Errorf("transient errors must not set channelOK/groupOK, got channelOK=%v groupOK=%v", channelOK, groupOK)
	}

	// Caller (tick) would thus skip clearWarned — simulate by not calling it.
	if sc.canWarn(G) {
		t.Fatal("cooldown must be preserved through transient errors")
	}
}
