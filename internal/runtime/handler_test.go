package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/herbertgao/group-limit-bot/internal/commands"
	"github.com/herbertgao/group-limit-bot/internal/gating"
	"github.com/herbertgao/group-limit-bot/internal/metrics"
	"github.com/herbertgao/group-limit-bot/internal/store"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

// handlerHarness builds the minimal wiring needed to exercise handleUpdate
// end-to-end: a real Gate (with seeded binding), Executor, and Dispatcher,
// plus a single recorded handler keyed to "/probe".
type handlerHarness struct {
	gate   *gating.Gate
	exec   *gating.Executor
	disp   *commands.Dispatcher
	mock   *telegram.MockClient
	log    *slog.Logger
	probed int
}

func newHandlerHarness(t *testing.T, groupID, channelID int64, memberStatus telegram.Status) *handlerHarness {
	return newHandlerHarnessRaw(t, groupID, channelID, memberStatus, true)
}

func newHandlerHarnessRaw(t *testing.T, groupID, channelID int64, memberStatus telegram.Status, seedBinding bool) *handlerHarness {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if seedBinding {
		if _, _, err := st.UpsertBinding(context.Background(), store.Binding{
			GroupChatID: groupID, ChannelChatID: channelID, BoundByUserID: 1, BoundAt: time.Unix(0, 0),
		}); err != nil {
			t.Fatal(err)
		}
	}

	mock := telegram.NewMockClient(telegram.User{ID: 777, IsBot: true, Username: "probe_bot"})
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return memberStatus, nil
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	cache := gating.NewMemberCache(st, 30*time.Minute)
	dedup := gating.NewMediaGroupDedup(60 * time.Second)
	reg := metrics.NewRegistry()
	disp := commands.NewDispatcher("probe_bot", log)

	cfg := gating.Config{
		TTL:           30 * time.Minute,
		IsCommandText: disp.IsPureCommand,
	}
	lookup := &runtimeStoreLookup{st: st}
	gate := gating.New(lookup, cache, dedup, mock, reg, log, cfg)
	exec := gating.NewExecutor(mock, reg, log)

	h := &handlerHarness{gate: gate, exec: exec, disp: disp, mock: mock, log: log}
	disp.Register("probe", func(ctx context.Context, msg *telego.Message, args string) error {
		h.probed++
		return nil
	})
	return h
}

type runtimeStoreLookup struct{ st *store.Store }

func (s *runtimeStoreLookup) Lookup(ctx context.Context, groupID int64) (*store.Binding, error) {
	return s.st.GetBinding(ctx, groupID)
}

func buildMessageUpdate(chatID, userID int64, messageID int, text string) telego.Update {
	return telego.Update{
		Message: &telego.Message{
			MessageID: messageID,
			Chat:      telego.Chat{ID: chatID, Type: "supergroup"},
			From:      &telego.User{ID: userID, IsBot: false},
			Text:      text,
		},
	}
}

// Regression: a non-channel-member sending a pure registered command must be
// deleted by the gate and must NOT invoke the command handler. Previously the
// dispatcher ran first, so /bind from a non-member triggered a "仅群创建者..."
// reply that lived in the chat for ~10s — exactly the moderation gap the
// missing message_ids (6154, 20428) in production logs pointed at.
func TestHandleUpdate_NonMemberCommandDeletedNotDispatched(t *testing.T) {
	const groupID, channelID, userID = int64(-1001), int64(-2001), int64(99)
	h := newHandlerHarness(t, groupID, channelID, telegram.StatusLeft)

	upd := buildMessageUpdate(groupID, userID, 4242, "/probe")
	handleUpdate(context.Background(), upd, h.gate, h.exec, h.disp, h.log)

	if h.probed != 0 {
		t.Errorf("dispatcher must not run for non-member; probed=%d", h.probed)
	}
	if len(h.mock.DeleteMessageCalls) != 1 || h.mock.DeleteMessageCalls[0].MessageID != 4242 {
		t.Errorf("expected gate to delete command(4242), got %+v", h.mock.DeleteMessageCalls)
	}
}

// A channel member sending the same registered command must NOT be deleted —
// the gate demotes Allow/Member to Ignore/Command and the dispatcher runs.
func TestHandleUpdate_MemberCommandDispatchedNotDeleted(t *testing.T) {
	const groupID, channelID, userID = int64(-1001), int64(-2001), int64(42)
	h := newHandlerHarness(t, groupID, channelID, telegram.StatusMember)

	upd := buildMessageUpdate(groupID, userID, 4243, "/probe")
	handleUpdate(context.Background(), upd, h.gate, h.exec, h.disp, h.log)

	if h.probed != 1 {
		t.Errorf("dispatcher must run exactly once for member; probed=%d", h.probed)
	}
	if len(h.mock.DeleteMessageCalls) != 0 {
		t.Errorf("gate must not delete a member's command; got %+v", h.mock.DeleteMessageCalls)
	}
}

// Pure command targeting a different bot (`/anything@otherbot`) follows the
// same rule: non-members get deleted, the gate's command short-circuit does
// not protect them anymore. This was the second bypass observed in logs.
func TestHandleUpdate_NonMemberOtherBotCommandDeleted(t *testing.T) {
	const groupID, channelID, userID = int64(-1001), int64(-2001), int64(99)
	h := newHandlerHarness(t, groupID, channelID, telegram.StatusLeft)

	upd := buildMessageUpdate(groupID, userID, 5151, "/buyfollowers@spambot")
	handleUpdate(context.Background(), upd, h.gate, h.exec, h.disp, h.log)

	if h.probed != 0 {
		t.Errorf("dispatcher must not run for non-member; probed=%d", h.probed)
	}
	if len(h.mock.DeleteMessageCalls) != 1 || h.mock.DeleteMessageCalls[0].MessageID != 5151 {
		t.Errorf("expected gate to delete other-bot command(5151), got %+v", h.mock.DeleteMessageCalls)
	}
}

// Regression: in an UNBOUND group, the gate returns Ignore/NoBinding. The
// initial /bind from a group creator must still reach the command dispatcher
// — otherwise no group can ever be configured. The previous fix gated dispatch
// on Reason==ReasonCommand only, which broke this setup path.
func TestHandleUpdate_UnboundGroupDispatchesCommand(t *testing.T) {
	const groupID, channelID, userID = int64(-1001), int64(-2001), int64(42)
	// memberStatus is irrelevant here — the gate short-circuits on no binding
	// before any GetChatMember call.
	h := newHandlerHarnessRaw(t, groupID, channelID, telegram.StatusMember, false)

	upd := buildMessageUpdate(groupID, userID, 7001, "/probe")
	handleUpdate(context.Background(), upd, h.gate, h.exec, h.disp, h.log)

	if h.probed != 1 {
		t.Errorf("dispatcher must run in unbound group so /bind can create initial binding; probed=%d", h.probed)
	}
	if len(h.mock.DeleteMessageCalls) != 0 {
		t.Errorf("gate must not delete in unbound group; got %+v", h.mock.DeleteMessageCalls)
	}
}

// Regression for adversarial-review [high]: when getChatMember fails (default
// allow), a pure command from a non-member must not reach the dispatcher.
// Previously demoteAllowedCommand rewrote Allow/ErrorDefaultAllow into
// Ignore/Command, so the runtime handed it to disp.Dispatch — re-opening the
// bot-amplification path during Telegram outages.
func TestHandleUpdate_GetChatMemberErrorDoesNotDispatch(t *testing.T) {
	const groupID, channelID, userID = int64(-1001), int64(-2001), int64(99)
	h := newHandlerHarness(t, groupID, channelID, telegram.StatusMember)
	h.mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusUnknown, errors.New("api outage")
	}
	upd := buildMessageUpdate(groupID, userID, 7777, "/probe")
	handleUpdate(context.Background(), upd, h.gate, h.exec, h.disp, h.log)
	if h.probed != 0 {
		t.Errorf("dispatcher must not run during membership API error; probed=%d", h.probed)
	}
	if len(h.mock.DeleteMessageCalls) != 0 {
		t.Errorf("gate must not delete on default-allow; got %+v", h.mock.DeleteMessageCalls)
	}
}
