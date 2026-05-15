package gating

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/herbertgao/group-limit-bot/internal/metrics"
	"github.com/herbertgao/group-limit-bot/internal/store"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

func newExecTest(t *testing.T) (*Executor, *telegram.MockClient, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mock := telegram.NewMockClient(telegram.User{ID: tBotID, IsBot: true})
	mock.DeleteMessageFn = func(ctx context.Context, chatID int64, mid int) error { return nil }
	exec := NewExecutor(mock, metrics.NewRegistry(), testLogger(), st, GuestPunishPolicy{
		MuteThreshold: 2, BanThreshold: 4, MuteDuration: time.Hour,
	})
	return exec, mock, st
}

func guestOutcome(callerID int64, summonID int) Outcome {
	return Outcome{
		Decision: DecisionDelete, Reason: ReasonGuestBot, GuestReply: true,
		GuestSummonMsgID: summonID, GuestCallerID: callerID,
		Binding: &store.Binding{GroupChatID: tGroupID, ChannelChatID: tChannelID},
	}
}

func TestExecutor_GuestFirstViolationNotPunished(t *testing.T) {
	exec, mock, _ := newExecTest(t)
	exec.Apply(context.Background(), tGroupID, 20, 8888, guestOutcome(tUserMember, 19), false, 0)
	mock.LockForTest()
	defer mock.UnlockForTest()
	if len(mock.RestrictCalls) != 0 || len(mock.BanCalls) != 0 {
		t.Errorf("first violation must not punish: restrict=%d ban=%d", len(mock.RestrictCalls), len(mock.BanCalls))
	}
	if len(mock.DeleteMessageCalls) != 2 {
		t.Errorf("expected ad + summon deleted, got %d deletes", len(mock.DeleteMessageCalls))
	}
}

func TestExecutor_GuestMuteAtThreshold(t *testing.T) {
	exec, mock, _ := newExecTest(t)
	ctx := context.Background()
	exec.Apply(ctx, tGroupID, 20, 8888, guestOutcome(tUserMember, 0), false, 0) // count 1
	exec.Apply(ctx, tGroupID, 21, 8888, guestOutcome(tUserMember, 0), false, 0) // count 2 -> mute
	mock.LockForTest()
	defer mock.UnlockForTest()
	if len(mock.RestrictCalls) != 1 {
		t.Errorf("expected 1 mute at threshold 2, got %d", len(mock.RestrictCalls))
	}
}

func TestExecutor_GuestBanAtThreshold(t *testing.T) {
	exec, mock, _ := newExecTest(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ { // counts 1..4; ban at 4
		exec.Apply(ctx, tGroupID, 20+i, 8888, guestOutcome(tUserMember, 0), false, 0)
	}
	mock.LockForTest()
	defer mock.UnlockForTest()
	if len(mock.BanCalls) != 1 {
		t.Errorf("expected 1 ban at threshold 4, got %d", len(mock.BanCalls))
	}
}

func TestExecutor_GuestEditedNotCounted(t *testing.T) {
	exec, _, st := newExecTest(t)
	ctx := context.Background()
	exec.Apply(ctx, tGroupID, 20, 8888, guestOutcome(tUserMember, 0), true, 0) // isEdit
	if n, _ := st.GetGuestViolation(ctx, tGroupID, tUserMember); n != 0 {
		t.Errorf("edited guest reply must not increment count, got %d", n)
	}
}

func TestExecutor_GuestNoCallerNotCounted(t *testing.T) {
	exec, _, st := newExecTest(t)
	ctx := context.Background()
	exec.Apply(ctx, tGroupID, 20, 8888, guestOutcome(0, 0), false, 0) // caller 0
	if n, _ := st.GetGuestViolation(ctx, tGroupID, 0); n != 0 {
		t.Errorf("missing caller must not be counted, got %d", n)
	}
}

func TestExecutor_GuestSummonDeleteErrorTolerated(t *testing.T) {
	exec, mock, _ := newExecTest(t)
	calls := 0
	mock.DeleteMessageFn = func(ctx context.Context, chatID int64, mid int) error {
		calls++
		if mid == 19 {
			return errors.New("Bad Request: message to delete not found")
		}
		return nil
	}
	exec.Apply(context.Background(), tGroupID, 20, 8888, guestOutcome(tUserMember, 19), false, 0)
	if calls != 2 {
		t.Errorf("expected ad + summon delete attempts, got %d", calls)
	}
}

func TestExecutor_GuestPunishFailureTolerated(t *testing.T) {
	exec, mock, _ := newExecTest(t)
	mock.RestrictChatMemberFn = func(ctx context.Context, chatID, userID int64, until time.Time) error {
		return errors.New("Bad Request: user is an administrator of the chat")
	}
	ctx := context.Background()
	exec.Apply(ctx, tGroupID, 20, 8888, guestOutcome(tUserMember, 0), false, 0)
	exec.Apply(ctx, tGroupID, 21, 8888, guestOutcome(tUserMember, 0), false, 0) // mute attempt fails
	mock.LockForTest()
	defer mock.UnlockForTest()
	if len(mock.RestrictCalls) != 1 {
		t.Errorf("expected restrict attempted once despite failure, got %d", len(mock.RestrictCalls))
	}
}
