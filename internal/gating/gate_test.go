package gating

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/herbertgao/group-limit-bot/internal/metrics"
	"github.com/herbertgao/group-limit-bot/internal/store"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

const (
	tGroupID    int64 = -1001
	tChannelID  int64 = -2001
	tOtherChan  int64 = -2999
	tBotID      int64 = 7
	tUserMember int64 = 42
	tUserNone   int64 = 99
)

func setup(t *testing.T) (*Gate, *telegram.MockClient, *MemberCache, *metrics.Registry, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// seed binding
	if _, _, err := st.UpsertBinding(context.Background(), store.Binding{
		GroupChatID: tGroupID, ChannelChatID: tChannelID, BoundByUserID: 1, BoundAt: time.Unix(0, 0),
	}); err != nil {
		t.Fatal(err)
	}

	mock := telegram.NewMockClient(telegram.User{ID: tBotID, IsBot: true})
	cache := NewMemberCache(st, 30*time.Minute)
	dedup := NewMediaGroupDedup(60 * time.Second)
	reg := metrics.NewRegistry()
	log := testLogger()

	lookup := &storeLookup{st: st}
	g := New(lookup, cache, dedup, mock, reg, log, Config{
		TTL:                 30 * time.Minute,
		AllowAnonymousAdmin: true,
	})
	return g, mock, cache, reg, st
}

type storeLookup struct{ st *store.Store }

func (s *storeLookup) Lookup(ctx context.Context, groupID int64) (*store.Binding, error) {
	return s.st.GetBinding(ctx, groupID)
}

func msgFromUser(userID int64, text string) *telego.Message {
	return &telego.Message{
		MessageID: 10,
		Chat:      telego.Chat{ID: tGroupID, Type: "supergroup"},
		From:      &telego.User{ID: userID, IsBot: false},
		Text:      text,
	}
}

func TestDecide_NoBinding(t *testing.T) {
	g, _, _, _, st := setup(t)
	_, _ = st.DeleteBinding(context.Background(), tGroupID)

	out := g.Decide(context.Background(), msgFromUser(tUserMember, "hi"), false)
	if out.Decision != DecisionIgnore || out.Reason != ReasonNoBinding {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
}

func TestDecide_NonMemberDeleted(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusLeft, nil
	}
	out := g.Decide(context.Background(), msgFromUser(tUserNone, "广告"), false)
	if out.Decision != DecisionDelete || out.Reason != ReasonNotMember {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
}

func TestDecide_KickedDeleted(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusBanned, nil
	}
	out := g.Decide(context.Background(), msgFromUser(tUserNone, "spam"), false)
	if out.Decision != DecisionDelete {
		t.Errorf("got %s", out.Decision)
	}
}

func TestDecide_MemberAllowedAndCached(t *testing.T) {
	g, mock, cache, _, _ := setup(t)
	calls := 0
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		calls++
		return telegram.StatusMember, nil
	}

	out1 := g.Decide(context.Background(), msgFromUser(tUserMember, "first"), false)
	if out1.Decision != DecisionAllow || out1.Reason != ReasonMember {
		t.Errorf("first: got %s/%s", out1.Decision, out1.Reason)
	}
	if calls != 1 {
		t.Errorf("expected 1 GetChatMember call, got %d", calls)
	}

	// Second message should hit cache.
	out2 := g.Decide(context.Background(), msgFromUser(tUserMember, "second"), false)
	if out2.Decision != DecisionAllow || out2.Reason != ReasonCacheHit {
		t.Errorf("second: got %s/%s", out2.Decision, out2.Reason)
	}
	if calls != 1 {
		t.Errorf("cache miss: GetChatMember called %d times total", calls)
	}

	// Sanity: cache reports the entry.
	hit, err := cache.Get(context.Background(), tGroupID, tChannelID, tUserMember, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Error("expected cache hit")
	}
}

func TestDecide_CacheExpiryRevalidates(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	calls := 0
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		calls++
		return telegram.StatusMember, nil
	}

	start := time.Unix(10_000, 0)
	g.now = func() time.Time { return start }
	out1 := g.Decide(context.Background(), msgFromUser(tUserMember, "a"), false)
	if out1.Reason != ReasonMember {
		t.Fatalf("first reason = %s", out1.Reason)
	}

	// Advance beyond TTL — cache must miss.
	g.now = func() time.Time { return start.Add(31 * time.Minute) }
	out2 := g.Decide(context.Background(), msgFromUser(tUserMember, "b"), false)
	if out2.Reason != ReasonMember {
		t.Errorf("expected revalidation Member, got %s", out2.Reason)
	}
	if calls != 2 {
		t.Errorf("expected 2 GetChatMember calls after TTL expiry, got %d", calls)
	}
}

func TestDecide_ChannelRootPostAllowed(t *testing.T) {
	g, _, _, _, _ := setup(t)
	msg := &telego.Message{
		MessageID:  1,
		Chat:       telego.Chat{ID: tGroupID, Type: "supergroup"},
		SenderChat: &telego.Chat{ID: tChannelID, Type: "channel"},
	}
	out := g.Decide(context.Background(), msg, false)
	if out.Decision != DecisionAllow || out.Reason != ReasonChannelRootPost {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
}

func TestDecide_OtherSenderChatDeleted(t *testing.T) {
	g, _, _, _, _ := setup(t)
	msg := &telego.Message{
		MessageID:  1,
		Chat:       telego.Chat{ID: tGroupID, Type: "supergroup"},
		SenderChat: &telego.Chat{ID: tOtherChan, Type: "channel"},
	}
	out := g.Decide(context.Background(), msg, false)
	if out.Decision != DecisionDelete || out.Reason != ReasonOtherSenderChat {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
}

func TestDecide_CommandAsOtherChannelIdentityPreserved(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	g.cfg.IsCommandText = func(s string) bool { return s == "/status@other_bot" }
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		t.Fatalf("GetChatMember must not be called for pure command as other channel identity: chat=%d user=%d", chatID, userID)
		return telegram.StatusUnknown, nil
	}
	msg := &telego.Message{
		MessageID:  1,
		Chat:       telego.Chat{ID: tGroupID, Type: "supergroup"},
		SenderChat: &telego.Chat{ID: tOtherChan, Type: "channel"},
		Text:       "/status@other_bot",
	}
	out := g.Decide(context.Background(), msg, false)
	if out.Decision != DecisionIgnore || out.Reason != ReasonCommand {
		t.Errorf("got %s/%s, want ignore/command", out.Decision, out.Reason)
	}
	if len(mock.GetChatMemberCalls) != 0 {
		t.Errorf("expected 0 GetChatMember calls, got %d", len(mock.GetChatMemberCalls))
	}
}

func TestDecide_ChannelRootPostCommandTextPreserved(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	g.cfg.IsCommandText = func(s string) bool { return s == "/bind" }
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		t.Fatalf("GetChatMember must not be called for channel root post with pure command text: chat=%d user=%d", chatID, userID)
		return telegram.StatusUnknown, nil
	}
	msg := &telego.Message{
		MessageID:  1,
		Chat:       telego.Chat{ID: tGroupID, Type: "supergroup"},
		SenderChat: &telego.Chat{ID: tChannelID, Type: "channel"},
		Text:       "/bind",
	}
	out := g.Decide(context.Background(), msg, false)
	// Command short-circuit wins over the sender_chat==bound_channel path; the
	// message is still preserved, just via ReasonCommand instead of ReasonChannelRootPost.
	if out.Decision != DecisionIgnore || out.Reason != ReasonCommand {
		t.Errorf("got %s/%s, want ignore/command", out.Decision, out.Reason)
	}
	if len(mock.GetChatMemberCalls) != 0 {
		t.Errorf("expected 0 GetChatMember calls, got %d", len(mock.GetChatMemberCalls))
	}
}

func TestDecide_EditedNonMemberDeleted(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusLeft, nil
	}
	out := g.Decide(context.Background(), msgFromUser(tUserNone, "edited to scam"), true)
	if out.Decision != DecisionDelete {
		t.Errorf("edited message should be deleted, got %s", out.Decision)
	}
}

func TestDecide_AnonymousAdminAllowed(t *testing.T) {
	g, _, _, _, _ := setup(t)
	// Realistic Telegram shape: sender_chat is the group itself, from is GroupAnonymousBot.
	msg := msgFromUser(telegram.GroupAnonymousBotID, "hi")
	msg.From.IsBot = true
	msg.SenderChat = &telego.Chat{ID: tGroupID, Type: "supergroup"}
	out := g.Decide(context.Background(), msg, false)
	if out.Decision != DecisionAllow || out.Reason != ReasonAnonymousAdmin {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
}

func TestDecide_AnonymousAdminDeletedWhenDisabled(t *testing.T) {
	g, _, _, _, _ := setup(t)
	g.cfg.AllowAnonymousAdmin = false
	msg := msgFromUser(telegram.GroupAnonymousBotID, "hi")
	msg.From.IsBot = true
	msg.SenderChat = &telego.Chat{ID: tGroupID, Type: "supergroup"}
	out := g.Decide(context.Background(), msg, false)
	if out.Decision != DecisionDelete || out.Reason != ReasonOtherSenderChat {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
}

func TestDecide_AnonymousBotFromIDWithOtherChannelSenderDeleted(t *testing.T) {
	g, _, _, _, _ := setup(t)
	msg := &telego.Message{
		MessageID:  1,
		Chat:       telego.Chat{ID: tGroupID, Type: "supergroup"},
		From:       &telego.User{ID: telegram.GroupAnonymousBotID, IsBot: true},
		SenderChat: &telego.Chat{ID: tOtherChan, Type: "channel"},
	}
	out := g.Decide(context.Background(), msg, false)
	if out.Decision != DecisionDelete || out.Reason != ReasonOtherSenderChat {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
}

func TestDecide_VideoChatServiceMessageIgnored(t *testing.T) {
	g, _, _, _, _ := setup(t)
	msg := &telego.Message{
		MessageID:        1,
		Chat:             telego.Chat{ID: tGroupID, Type: "supergroup"},
		VideoChatStarted: &telego.VideoChatStarted{},
	}
	out := g.Decide(context.Background(), msg, false)
	if out.Decision != DecisionIgnore || out.Reason != ReasonServiceMessage {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
}

func TestDecide_BotAllowlistAllowed(t *testing.T) {
	g, _, _, _, _ := setup(t)
	g.cfg.BotAllowlist = map[int64]bool{555: true}
	msg := msgFromUser(555, "hi")
	msg.From.IsBot = true
	out := g.Decide(context.Background(), msg, false)
	if out.Decision != DecisionAllow || out.Reason != ReasonBotAllowlist {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
}

func TestDecide_APIErrorDefaultsAllow(t *testing.T) {
	g, mock, _, reg, _ := setup(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusUnknown, errors.New("boom")
	}
	out := g.Decide(context.Background(), msgFromUser(tUserNone, "x"), false)
	if out.Decision != DecisionAllow || out.Reason != ReasonErrorDefaultAllow {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
	if len(reg.RecentErrors()) != 1 {
		t.Errorf("expected 1 recorded error, got %d", len(reg.RecentErrors()))
	}
}

func TestDecide_RateLimitErrorPopulatesRetryAfter(t *testing.T) {
	g, mock, _, reg, _ := setup(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusUnknown, &telegram.RateLimitError{ChatID: tChannelID, RetryAfter: 7 * time.Second}
	}
	out := g.Decide(context.Background(), msgFromUser(tUserNone, "x"), false)
	if out.Decision != DecisionAllow || out.Reason != ReasonErrorDefaultAllow {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
	errs := reg.RecentErrorsForGroup(tGroupID)
	if len(errs) != 1 {
		t.Fatalf("expected 1 recorded error for group, got %d", len(errs))
	}
	if errs[0].RetryAfter != 7*time.Second {
		t.Errorf("expected RetryAfter=7s, got %s", errs[0].RetryAfter)
	}
}

func TestDecide_MediaGroupDedup(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	calls := 0
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		calls++
		return telegram.StatusLeft, nil
	}
	group := "MG-1"
	for i := 0; i < 4; i++ {
		m := msgFromUser(tUserNone, "photo")
		m.MessageID = 100 + i
		m.MediaGroupID = group
		out := g.Decide(context.Background(), m, false)
		if out.Decision != DecisionDelete {
			t.Errorf("media %d: got %s", i, out.Decision)
		}
	}
	if calls != 1 {
		t.Errorf("expected 1 GetChatMember across media group, got %d", calls)
	}
}

func TestDecide_MediaGroupErrorDedupedOnce(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	calls := 0
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		calls++
		return telegram.StatusUnknown, errors.New("boom")
	}
	group := "MG-err"
	for i := 0; i < 4; i++ {
		m := msgFromUser(tUserNone, "photo")
		m.MessageID = 200 + i
		m.MediaGroupID = group
		out := g.Decide(context.Background(), m, false)
		if out.Decision != DecisionAllow {
			t.Errorf("media %d: decision got %s, want allow", i, out.Decision)
		}
		if i == 0 {
			if out.Reason != ReasonErrorDefaultAllow {
				t.Errorf("first: reason got %s, want %s", out.Reason, ReasonErrorDefaultAllow)
			}
		} else {
			if out.Reason != ReasonMediaGroupDedup {
				t.Errorf("media %d: reason got %s, want %s", i, out.Reason, ReasonMediaGroupDedup)
			}
		}
	}
	if calls != 1 {
		t.Errorf("expected 1 GetChatMember across media group, got %d", calls)
	}
}

func TestDecide_EditedMessageBypassesMediaGroupDedup(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	calls := 0
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		calls++
		if calls == 1 {
			return telegram.StatusLeft, nil
		}
		return telegram.StatusMember, nil
	}

	first := msgFromUser(tUserMember, "photo")
	first.MessageID = 300
	first.MediaGroupID = "album-1"
	out1 := g.Decide(context.Background(), first, false)
	if out1.Decision != DecisionDelete || out1.Reason != ReasonNotMember {
		t.Fatalf("first: got %s/%s, want delete/not_member", out1.Decision, out1.Reason)
	}

	edited := msgFromUser(tUserMember, "photo edited")
	edited.MessageID = 300
	edited.MediaGroupID = "album-1"
	out2 := g.Decide(context.Background(), edited, true)
	if out2.Decision != DecisionAllow || out2.Reason != ReasonMember {
		t.Fatalf("edit: got %s/%s, want allow/member", out2.Decision, out2.Reason)
	}
	if calls != 2 {
		t.Errorf("expected 2 GetChatMember calls (dedup skipped on edit), got %d", calls)
	}
	if len(mock.GetChatMemberCalls) != 2 {
		t.Errorf("expected 2 recorded GetChatMember calls, got %d", len(mock.GetChatMemberCalls))
	}
}

func TestDecide_ServiceMessageIgnored(t *testing.T) {
	g, _, _, _, _ := setup(t)
	msg := &telego.Message{
		MessageID:      1,
		Chat:           telego.Chat{ID: tGroupID, Type: "supergroup"},
		NewChatMembers: []telego.User{{ID: 50}},
	}
	out := g.Decide(context.Background(), msg, false)
	if out.Decision != DecisionIgnore || out.Reason != ReasonServiceMessage {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
}

func TestDecide_CommandMessageIgnored(t *testing.T) {
	g, _, _, _, _ := setup(t)
	g.cfg.IsCommandText = func(text string) bool { return text == "/status" }
	out := g.Decide(context.Background(), msgFromUser(tUserNone, "/status"), false)
	if out.Decision != DecisionIgnore || out.Reason != ReasonCommand {
		t.Errorf("got %s/%s", out.Decision, out.Reason)
	}
}

func TestDecide_CommandToOtherBotBypassesModeration(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	g.cfg.IsCommandText = func(text string) bool {
		return text == "/status" || text == "/status@other_bot"
	}
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		t.Fatalf("GetChatMember must not be called for pure command to other bot: chat=%d user=%d", chatID, userID)
		return telegram.StatusUnknown, nil
	}
	out := g.Decide(context.Background(), msgFromUser(tUserNone, "/status@other_bot"), false)
	if out.Decision != DecisionIgnore || out.Reason != ReasonCommand {
		t.Errorf("got %s/%s, want ignore/command", out.Decision, out.Reason)
	}
	if len(mock.GetChatMemberCalls) != 0 {
		t.Errorf("expected 0 GetChatMember calls, got %d", len(mock.GetChatMemberCalls))
	}
}

func TestDecide_MalformedCommandDoesNotBypass(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	// Simulate the fixed dispatcher: malformed @ suffix is NOT a command.
	g.cfg.IsCommandText = func(s string) bool { return false }
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusLeft, nil
	}
	out := g.Decide(context.Background(), msgFromUser(tUserNone, "/status@"), false)
	if out.Decision != DecisionDelete || out.Reason != ReasonNotMember {
		t.Errorf("got %s/%s, want delete/not_member", out.Decision, out.Reason)
	}
}

func TestDecide_EditedMessageLooksLikeCommandStillModerated(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	g.cfg.IsCommandText = func(s string) bool { return s == "/status" }
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusLeft, nil
	}
	out := g.Decide(context.Background(), msgFromUser(tUserNone, "/status"), true)
	if out.Decision != DecisionDelete || out.Reason != ReasonNotMember {
		t.Errorf("got %s/%s, want delete/not_member", out.Decision, out.Reason)
	}
}

// A caption containing command-like text must never trigger the command
// short-circuit. Media with caption "/status" is a media post, not a command.
func TestDecide_CaptionWithCommandNotShortCircuited(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	g.cfg.IsCommandText = func(s string) bool { return s == "/status" }
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusLeft, nil
	}
	msg := &telego.Message{
		MessageID: 11,
		Chat:      telego.Chat{ID: tGroupID, Type: "supergroup"},
		From:      &telego.User{ID: tUserNone, IsBot: false},
		Text:      "",
		Caption:   "/status",
	}
	out := g.Decide(context.Background(), msg, false)
	if out.Decision != DecisionDelete || out.Reason != ReasonNotMember {
		t.Errorf("got %s/%s, want delete/not_member", out.Decision, out.Reason)
	}
}

// A message whose text has a leading space before `/cmd` must NOT be treated
// as a pure command by the gate, because the dispatcher's strict `parse` also
// refuses leading whitespace. Keeping the two in lockstep prevents a message
// from escaping moderation without any handler ever running.
func TestDecide_LeadingWhitespaceCommandNotShortCircuited(t *testing.T) {
	g, mock, _, _, _ := setup(t)
	g.cfg.IsCommandText = func(s string) bool { return s == "/status" }
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusLeft, nil
	}
	out := g.Decide(context.Background(), msgFromUser(tUserNone, " /status"), false)
	if out.Decision != DecisionDelete || out.Reason != ReasonNotMember {
		t.Errorf("got %s/%s, want delete/not_member", out.Decision, out.Reason)
	}
}

// Approval recorded against one channel must not satisfy a lookup under a
// different channel after the group is rebound.
func TestDecide_CacheIsChannelScoped(t *testing.T) {
	g, mock, cache, _, st := setup(t)
	ctx := context.Background()
	now := time.Unix(10_000, 0)
	g.now = func() time.Time { return now }

	// Seed cache against the original channel (tChannelID).
	origBinding, err := st.GetBinding(ctx, tGroupID)
	if err != nil || origBinding == nil {
		t.Fatalf("get orig binding: %v", err)
	}
	if err := cache.Set(ctx, tGroupID, tChannelID, tUserMember, origBinding.Epoch, now); err != nil {
		t.Fatal(err)
	}

	// Rebind group to a different channel; cascade wipes SQLite rows for this group.
	newChannel := int64(-2500)
	if _, _, err := st.UpsertBinding(ctx, store.Binding{
		GroupChatID: tGroupID, ChannelChatID: newChannel, BoundByUserID: 1, BoundAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}

	// Gate must MISS cache (different channel key) and fall back to GetChatMember.
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusMember, nil
	}

	out := g.Decide(ctx, msgFromUser(tUserMember, "hi"), false)
	if out.Decision != DecisionAllow || out.Reason != ReasonMember {
		t.Errorf("want allow/member, got %s/%s", out.Decision, out.Reason)
	}
	if len(mock.GetChatMemberCalls) != 1 {
		t.Fatalf("expected 1 GetChatMember call on channel-scoped miss, got %d", len(mock.GetChatMemberCalls))
	}
	if c := mock.GetChatMemberCalls[0]; c.ChatID != newChannel || c.UserID != tUserMember {
		t.Errorf("expected call (%d,%d), got %+v", newChannel, tUserMember, c)
	}
}

// When the SQLite store write fails, the in-memory cache must NOT be updated,
// so subsequent Get calls still miss.
func TestDecide_CacheSetFailureDoesNotPopulateMemory(t *testing.T) {
	_, _, cache, _, st := setup(t)
	ctx := context.Background()
	now := time.Unix(20_000, 0)

	// Force store writes to fail.
	_ = st.Close()

	err := cache.Set(ctx, tGroupID, tChannelID, tUserMember, 1, now)
	if err == nil {
		t.Fatal("expected Set to fail after store close")
	}
	// Memory must not have been populated.
	// Call Get — with closed store the SQLite fallback will error, which is fine;
	// what matters is that the in-memory map does not contain the key.
	hit, _ := cache.Get(ctx, tGroupID, tChannelID, tUserMember, now)
	if hit {
		t.Error("cache memory must not be populated when SQLite Set failed")
	}
}
