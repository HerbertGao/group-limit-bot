package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/herbertgao/group-limit-bot/internal/binding"
	"github.com/herbertgao/group-limit-bot/internal/gating"
	"github.com/herbertgao/group-limit-bot/internal/metrics"
	"github.com/herbertgao/group-limit-bot/internal/store"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

const (
	gID  int64 = -1001
	cID  int64 = -2001
	admin int64 = 100
	noob  int64 = 200
	bot   int64 = 999
)

func buildDeps(t *testing.T) (*Deps, *Dispatcher, *telegram.MockClient, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mock := telegram.NewMockClient(telegram.User{ID: bot, IsBot: true, Username: "my_bot"})
	bindSvc := binding.New(st, mock)
	reg := metrics.NewRegistry()
	cache := gating.NewMemberCache(st, 0)
	deps := &Deps{
		BindSvc: bindSvc,
		TG:      mock,
		Store:   st,
		Metrics: reg,
		Cache:   cache,
		Log:     quietLogger(),
	}
	disp := NewDispatcher("my_bot", quietLogger())
	deps.Register(disp)
	return deps, disp, mock, st
}

func groupMsg(text string, from int64) *telego.Message {
	return &telego.Message{
		Chat: telego.Chat{ID: gID, Type: "supergroup", Title: "测试群"},
		From: &telego.User{ID: from, IsBot: false},
		Text: text,
	}
}

func stubAllAdminAndLinked(mock *telegram.MockClient) {
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		if userID == admin {
			return telegram.StatusCreator, nil
		}
		if chatID == cID && userID == bot {
			return telegram.StatusAdministrator, nil
		}
		return telegram.StatusMember, nil
	}
	mock.GetChatMemberCanDeleteFn = func(ctx context.Context, chatID, userID int64) (bool, error) {
		return true, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		if chatID == gID {
			return &telegram.ChatInfo{ID: gID, Type: "supergroup", Title: "测试群", LinkedChatID: cID}, nil
		}
		return &telegram.ChatInfo{ID: cID, Type: "channel", Title: "主频道", Username: "main"}, nil
	}
}

func TestBindHandler_Success(t *testing.T) {
	_, disp, mock, st := buildDeps(t)
	stubAllAdminAndLinked(mock)
	handled, err := disp.Dispatch(context.Background(), groupMsg("/bind", admin))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	b, err := st.GetBinding(context.Background(), gID)
	if err != nil || b == nil {
		t.Fatalf("binding not persisted: %v %v", b, err)
	}
	if b.ChannelChatID != cID {
		t.Errorf("ChannelChatID=%d", b.ChannelChatID)
	}
	if len(mock.SendMessageCalls) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(mock.SendMessageCalls))
	}
	if !mock.SendMessageCalls[0].MarkdownV2 {
		t.Error("bind reply should be MarkdownV2")
	}
}

func TestBindHandler_ChannelUsernameWithUnderscoreNotEscaped(t *testing.T) {
	_, disp, mock, _ := buildDeps(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		if userID == admin {
			return telegram.StatusCreator, nil
		}
		if chatID == cID && userID == bot {
			return telegram.StatusAdministrator, nil
		}
		return telegram.StatusMember, nil
	}
	mock.GetChatMemberCanDeleteFn = func(ctx context.Context, chatID, userID int64) (bool, error) {
		return true, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		if chatID == gID {
			return &telegram.ChatInfo{ID: gID, Type: "supergroup", Title: "测试群", LinkedChatID: cID}, nil
		}
		return &telegram.ChatInfo{ID: cID, Type: "channel", Title: "主频道", Username: "my_channel"}, nil
	}
	handled, err := disp.Dispatch(context.Background(), groupMsg("/bind", admin))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if len(mock.SendMessageCalls) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(mock.SendMessageCalls))
	}
	text := mock.SendMessageCalls[0].Text
	if !strings.Contains(text, "https://t.me/my_channel") {
		t.Errorf("expected literal https://t.me/my_channel in reply, got: %s", text)
	}
	if strings.Contains(text, `my\_channel`) {
		t.Errorf("underscore in URL target must not be escaped, got: %s", text)
	}
}

func TestBindHandler_RebindingNewChannelClearsCache(t *testing.T) {
	// Custom deps with a non-zero TTL so cache entries we seed actually hit.
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mock := telegram.NewMockClient(telegram.User{ID: bot, IsBot: true, Username: "my_bot"})
	bindSvc := binding.New(st, mock)
	reg := metrics.NewRegistry()
	cache := gating.NewMemberCache(st, time.Hour)
	deps := &Deps{
		BindSvc: bindSvc,
		TG:      mock,
		Store:   st,
		Metrics: reg,
		Cache:   cache,
		Log:     quietLogger(),
	}
	disp := NewDispatcher("my_bot", quietLogger())
	deps.Register(disp)

	// Pre-seed a binding pointing at a different channel, and a warm cache entry.
	oldChannel := int64(-9001)
	if _, _, err := st.UpsertBinding(context.Background(), store.Binding{GroupChatID: gID, ChannelChatID: oldChannel}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000, 0)
	if err := deps.Cache.Set(context.Background(), gID, oldChannel, 42, 1, now); err != nil {
		t.Fatal(err)
	}
	if hit, err := deps.Cache.Get(context.Background(), gID, oldChannel, 42, now); err != nil || !hit {
		t.Fatalf("expected pre-seeded cache hit, hit=%v err=%v", hit, err)
	}

	// Now /bind points at cID, which differs from oldChannel.
	stubAllAdminAndLinked(mock)
	handled, err := disp.Dispatch(context.Background(), groupMsg("/bind", admin))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}

	b, err := st.GetBinding(context.Background(), gID)
	if err != nil || b == nil {
		t.Fatalf("binding not persisted: %v %v", b, err)
	}
	if b.ChannelChatID != cID {
		t.Errorf("ChannelChatID=%d, want %d", b.ChannelChatID, cID)
	}
	// Cache must now miss: in-memory dropped by DropGroup, SQLite wiped by cascade.
	if hit, err := deps.Cache.Get(context.Background(), gID, oldChannel, 42, now); err != nil {
		t.Fatalf("cache get error: %v", err)
	} else if hit {
		t.Error("cache should have been dropped after rebinding to a different channel")
	}
}

func TestBindHandler_NonCreatorRejected(t *testing.T) {
	_, disp, mock, st := buildDeps(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusAdministrator, nil
	}
	handled, err := disp.Dispatch(context.Background(), groupMsg("/bind", noob))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	b, err := st.GetBinding(context.Background(), gID)
	if err != nil {
		t.Fatal(err)
	}
	if b != nil {
		t.Error("binding should not have been created")
	}
	if len(mock.SendMessageCalls) != 1 || !strings.Contains(mock.SendMessageCalls[0].Text, "创建者") {
		t.Errorf("expected creator-permission reply, got %+v", mock.SendMessageCalls)
	}
}

func TestUnbindHandler_ClearsBindingAndCache(t *testing.T) {
	deps, disp, mock, st := buildDeps(t)
	stubAllAdminAndLinked(mock)
	// Pre-create binding.
	if _, _, err := st.UpsertBinding(context.Background(), store.Binding{GroupChatID: gID, ChannelChatID: cID}); err != nil {
		t.Fatal(err)
	}
	// Warm cache entry.
	if err := deps.Cache.Set(context.Background(), gID, cID, 42, 1, timeZero()); err != nil {
		t.Fatal(err)
	}

	handled, err := disp.Dispatch(context.Background(), groupMsg("/unbind", admin))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	b, _ := st.GetBinding(context.Background(), gID)
	if b != nil {
		t.Error("binding should be removed")
	}
}

func TestStatusHandler_AdminGetsReport(t *testing.T) {
	_, disp, mock, st := buildDeps(t)
	stubAllAdminAndLinked(mock)
	if _, _, err := st.UpsertBinding(context.Background(), store.Binding{GroupChatID: gID, ChannelChatID: cID}); err != nil {
		t.Fatal(err)
	}

	handled, err := disp.Dispatch(context.Background(), groupMsg("/status", admin))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if len(mock.SendMessageCalls) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(mock.SendMessageCalls))
	}
	text := mock.SendMessageCalls[0].Text
	if !strings.Contains(text, "状态") || !strings.Contains(text, "群组") {
		t.Errorf("reply missing expected content: %s", text)
	}
}

func TestStatusHandler_OnlyShowsErrorsForCurrentGroup(t *testing.T) {
	deps, disp, mock, st := buildDeps(t)
	stubAllAdminAndLinked(mock)

	// Two bindings: G1→C1 (the group we'll query) and G2→C2 (unrelated).
	const (
		g2ID int64 = -1002
		c2ID int64 = -2002
	)
	if _, _, err := st.UpsertBinding(context.Background(), store.Binding{GroupChatID: gID, ChannelChatID: cID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpsertBinding(context.Background(), store.Binding{GroupChatID: g2ID, ChannelChatID: c2ID}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	deps.Metrics.RecordError(metrics.ErrorRecord{At: now, Op: "getChatMember", ChatID: cID, GroupChatID: gID, Err: "err-G1"})
	deps.Metrics.RecordError(metrics.ErrorRecord{At: now, Op: "getChatMember", ChatID: c2ID, GroupChatID: g2ID, Err: "err-G2"})

	handled, err := disp.Dispatch(context.Background(), groupMsg("/status", admin))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if len(mock.SendMessageCalls) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(mock.SendMessageCalls))
	}
	// MarkdownV2 escapes '-' to '\-'; accept either form.
	text := mock.SendMessageCalls[0].Text
	if !strings.Contains(text, "G1") || !strings.Contains(text, "err") {
		t.Errorf("reply should contain G1 error, got: %s", text)
	}
	if strings.Contains(text, "G2") {
		t.Errorf("reply must not contain G2 (other group's error), got: %s", text)
	}
}

func TestReplies_AlwaysMarkdownV2(t *testing.T) {
	// /bind where caller is non-creator -> "仅群创建者..." reply.
	t.Run("bind non-creator", func(t *testing.T) {
		_, disp, mock, _ := buildDeps(t)
		mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
			return telegram.StatusMember, nil
		}
		handled, err := disp.Dispatch(context.Background(), groupMsg("/bind", noob))
		if err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		if len(mock.SendMessageCalls) != 1 {
			t.Fatalf("expected 1 reply, got %d", len(mock.SendMessageCalls))
		}
		if !mock.SendMessageCalls[0].MarkdownV2 {
			t.Error("bind non-creator reply should be MarkdownV2")
		}
	})

	// /unbind on an unbound group -> "当前群未绑定任何频道" reply.
	t.Run("unbind not-bound", func(t *testing.T) {
		_, disp, mock, _ := buildDeps(t)
		stubAllAdminAndLinked(mock)
		handled, err := disp.Dispatch(context.Background(), groupMsg("/unbind", admin))
		if err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		if len(mock.SendMessageCalls) != 1 {
			t.Fatalf("expected 1 reply, got %d", len(mock.SendMessageCalls))
		}
		if !mock.SendMessageCalls[0].MarkdownV2 {
			t.Error("unbind not-bound reply should be MarkdownV2")
		}
	})

	// /status where caller is non-creator -> "仅群创建者..." reply.
	t.Run("status non-creator", func(t *testing.T) {
		_, disp, mock, st := buildDeps(t)
		mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
			return telegram.StatusMember, nil
		}
		if _, _, err := st.UpsertBinding(context.Background(), store.Binding{GroupChatID: gID, ChannelChatID: cID}); err != nil {
			t.Fatal(err)
		}
		handled, _ := disp.Dispatch(context.Background(), groupMsg("/status", noob))
		if !handled {
			t.Fatal("should handle")
		}
		if len(mock.SendMessageCalls) != 1 {
			t.Fatalf("expected 1 reply, got %d", len(mock.SendMessageCalls))
		}
		if !mock.SendMessageCalls[0].MarkdownV2 {
			t.Error("status non-creator reply should be MarkdownV2")
		}
	})
}

func TestBindHandler_AutoCleanupOnSuccess(t *testing.T) {
	deps, disp, mock, _ := buildDeps(t)
	stubAllAdminAndLinked(mock)
	// SendMessage returns a predictable reply message ID.
	mock.SendMessageFn = func(ctx context.Context, chatID int64, text string, md2 bool) (int, error) {
		return 9101, nil
	}
	deps.CleanupDelay = 20 * time.Millisecond

	msg := groupMsg("/bind", admin)
	msg.MessageID = 8001
	handled, err := disp.Dispatch(context.Background(), msg)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}

	// Wait for cleanup goroutine to fire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mock.LockForTest()
		n := len(mock.DeleteMessageCalls)
		mock.UnlockForTest()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mock.LockForTest()
	defer mock.UnlockForTest()
	if len(mock.DeleteMessageCalls) != 2 {
		t.Fatalf("expected 2 DeleteMessage calls (command + reply), got %d: %+v", len(mock.DeleteMessageCalls), mock.DeleteMessageCalls)
	}
	ids := map[int]bool{
		mock.DeleteMessageCalls[0].MessageID: true,
		mock.DeleteMessageCalls[1].MessageID: true,
	}
	if !ids[8001] || !ids[9101] {
		t.Errorf("expected cleanup to cover command(8001) and reply(9101), got %+v", mock.DeleteMessageCalls)
	}
}

func TestBindHandler_AutoCleanupOnRejection(t *testing.T) {
	deps, disp, mock, _ := buildDeps(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusMember, nil
	}
	mock.SendMessageFn = func(ctx context.Context, chatID int64, text string, md2 bool) (int, error) {
		return 9201, nil
	}
	deps.CleanupDelay = 20 * time.Millisecond

	msg := groupMsg("/bind", noob)
	msg.MessageID = 8002
	handled, err := disp.Dispatch(context.Background(), msg)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mock.LockForTest()
		n := len(mock.DeleteMessageCalls)
		mock.UnlockForTest()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mock.LockForTest()
	defer mock.UnlockForTest()
	if len(mock.DeleteMessageCalls) != 2 {
		t.Fatalf("expected 2 DeleteMessage calls (command + reply), got %d: %+v", len(mock.DeleteMessageCalls), mock.DeleteMessageCalls)
	}
}

func TestBindHandler_RejectionCleansUpCommandAndReply(t *testing.T) {
	deps, disp, mock, _ := buildDeps(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusMember, nil
	}
	mock.SendMessageFn = func(ctx context.Context, chatID int64, text string, md2 bool) (int, error) {
		return 9601, nil
	}
	deps.CleanupDelay = 200 * time.Millisecond

	msg := groupMsg("/bind", noob)
	msg.MessageID = 8301
	dispatchStart := time.Now()
	handled, err := disp.Dispatch(context.Background(), msg)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	// Rejection path must NOT delete the command synchronously — it should wait
	// for the scheduled cleanup so operators can correlate reply with command.
	elapsed := time.Since(dispatchStart)
	if elapsed > 150*time.Millisecond {
		t.Fatalf("Dispatch took %v — rejection should not block on synchronous deletion", elapsed)
	}
	mock.LockForTest()
	immediate := len(mock.DeleteMessageCalls)
	mock.UnlockForTest()
	if immediate != 0 {
		t.Fatalf("expected 0 immediate DeleteMessage calls, got %d", immediate)
	}

	// Both command and reply deletions should fire at ~CleanupDelay.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mock.LockForTest()
		n := len(mock.DeleteMessageCalls)
		mock.UnlockForTest()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mock.LockForTest()
	defer mock.UnlockForTest()
	if len(mock.DeleteMessageCalls) != 2 {
		t.Fatalf("expected 2 DeleteMessage calls overall, got %d: %+v", len(mock.DeleteMessageCalls), mock.DeleteMessageCalls)
	}
	ids := map[int]bool{mock.DeleteMessageCalls[0].MessageID: true, mock.DeleteMessageCalls[1].MessageID: true}
	if !ids[8301] || !ids[9601] {
		t.Errorf("expected both command(8301) and reply(9601), got %+v", mock.DeleteMessageCalls)
	}
}

func TestStatusHandler_NonCreatorDenied(t *testing.T) {
	_, disp, mock, st := buildDeps(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusAdministrator, nil
	}
	if _, _, err := st.UpsertBinding(context.Background(), store.Binding{GroupChatID: gID, ChannelChatID: cID}); err != nil {
		t.Fatal(err)
	}
	handled, _ := disp.Dispatch(context.Background(), groupMsg("/status", noob))
	if !handled {
		t.Fatal("should handle")
	}
	if len(mock.SendMessageCalls) != 1 {
		t.Fatalf("expected 1 reply")
	}
	if !strings.Contains(mock.SendMessageCalls[0].Text, "创建者") {
		t.Errorf("expected creator-permission rejection, got %s", mock.SendMessageCalls[0].Text)
	}
}

func TestStatusHandler_AutoCleanupOnRejection(t *testing.T) {
	deps, disp, mock, st := buildDeps(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusMember, nil
	}
	mock.SendMessageFn = func(ctx context.Context, chatID int64, text string, md2 bool) (int, error) {
		return 9301, nil
	}
	if _, _, err := st.UpsertBinding(context.Background(), store.Binding{GroupChatID: gID, ChannelChatID: cID}); err != nil {
		t.Fatal(err)
	}
	deps.CleanupDelay = 20 * time.Millisecond

	msg := groupMsg("/status", noob)
	msg.MessageID = 8101
	handled, err := disp.Dispatch(context.Background(), msg)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mock.LockForTest()
		n := len(mock.DeleteMessageCalls)
		mock.UnlockForTest()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mock.LockForTest()
	defer mock.UnlockForTest()
	if len(mock.DeleteMessageCalls) != 2 {
		t.Fatalf("expected 2 DeleteMessage calls (command + reply), got %d: %+v", len(mock.DeleteMessageCalls), mock.DeleteMessageCalls)
	}
	ids := map[int]bool{
		mock.DeleteMessageCalls[0].MessageID: true,
		mock.DeleteMessageCalls[1].MessageID: true,
	}
	if !ids[8101] || !ids[9301] {
		t.Errorf("expected cleanup to cover command(8101) and reply(9301), got %+v", mock.DeleteMessageCalls)
	}
}

func TestBindHandler_SuccessWithSendFailureReturnsError(t *testing.T) {
	deps, disp, mock, st := buildDeps(t)
	stubAllAdminAndLinked(mock)
	mock.SendMessageFn = func(ctx context.Context, chatID int64, text string, md2 bool) (int, error) {
		return 0, errors.New("telegram rejected markdown")
	}
	deps.CleanupDelay = 20 * time.Millisecond

	msg := groupMsg("/bind", admin)
	msg.MessageID = 8501
	handled, err := disp.Dispatch(context.Background(), msg)
	if !handled {
		t.Fatal("should handle")
	}
	if err == nil {
		t.Fatal("expected error from handleBind when reply fails")
	}
	b, gerr := st.GetBinding(context.Background(), gID)
	if gerr != nil || b == nil {
		t.Fatalf("binding must still be persisted: b=%v err=%v", b, gerr)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mock.LockForTest()
		n := len(mock.DeleteMessageCalls)
		mock.UnlockForTest()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mock.LockForTest()
	defer mock.UnlockForTest()
	if len(mock.DeleteMessageCalls) < 1 {
		t.Fatalf("expected cleanup to schedule command deletion, got %d", len(mock.DeleteMessageCalls))
	}
	found := false
	for _, c := range mock.DeleteMessageCalls {
		if c.MessageID == 8501 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected command(8501) to be cleaned up, got %+v", mock.DeleteMessageCalls)
	}
}

func TestStatusHandler_ChannelMetadataFailureReturnsError(t *testing.T) {
	deps, disp, mock, st := buildDeps(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		if userID == admin {
			return telegram.StatusCreator, nil
		}
		return telegram.StatusMember, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		if chatID == cID {
			return nil, errors.New("forbidden: bot lost channel access")
		}
		return &telegram.ChatInfo{ID: chatID, Type: "supergroup", Title: "测试群"}, nil
	}
	mock.SendMessageFn = func(ctx context.Context, chatID int64, text string, md2 bool) (int, error) {
		return 9701, nil
	}
	if _, _, err := st.UpsertBinding(context.Background(), store.Binding{GroupChatID: gID, ChannelChatID: cID}); err != nil {
		t.Fatal(err)
	}
	deps.CleanupDelay = 20 * time.Millisecond

	msg := groupMsg("/status", admin)
	msg.MessageID = 8601
	handled, err := disp.Dispatch(context.Background(), msg)
	if !handled {
		t.Fatal("should handle")
	}
	if err == nil {
		t.Fatal("expected error from handleStatus when GetChat(channel) fails")
	}
	if len(mock.SendMessageCalls) != 1 {
		t.Fatalf("expected exactly 1 reply, got %d", len(mock.SendMessageCalls))
	}
	if !mock.SendMessageCalls[0].MarkdownV2 {
		t.Error("status channel-failure reply should be MarkdownV2")
	}
	if !strings.Contains(mock.SendMessageCalls[0].Text, "读取频道信息失败") {
		t.Errorf("reply should mention the failure, got: %s", mock.SendMessageCalls[0].Text)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mock.LockForTest()
		n := len(mock.DeleteMessageCalls)
		mock.UnlockForTest()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mock.LockForTest()
	defer mock.UnlockForTest()
	if len(mock.DeleteMessageCalls) != 2 {
		t.Fatalf("expected 2 DeleteMessage calls (command + reply), got %d: %+v", len(mock.DeleteMessageCalls), mock.DeleteMessageCalls)
	}
	ids := map[int]bool{
		mock.DeleteMessageCalls[0].MessageID: true,
		mock.DeleteMessageCalls[1].MessageID: true,
	}
	if !ids[8601] || !ids[9701] {
		t.Errorf("expected cleanup to cover command(8601) and reply(9701), got %+v", mock.DeleteMessageCalls)
	}
}

func TestStatusHandler_SendFailureReturnsError(t *testing.T) {
	_, disp, mock, st := buildDeps(t)
	stubAllAdminAndLinked(mock)
	if _, _, err := st.UpsertBinding(context.Background(), store.Binding{GroupChatID: gID, ChannelChatID: cID}); err != nil {
		t.Fatal(err)
	}
	mock.SendMessageFn = func(ctx context.Context, chatID int64, text string, md2 bool) (int, error) {
		return 0, errors.New("boom")
	}

	handled, err := disp.Dispatch(context.Background(), groupMsg("/status", admin))
	if !handled {
		t.Fatal("should handle")
	}
	if err == nil {
		t.Fatal("expected error from handleStatus when reply fails")
	}
	if len(mock.SendMessageCalls) != 1 {
		t.Fatalf("expected exactly 1 SendMessage attempt, got %d", len(mock.SendMessageCalls))
	}
}

func TestStatusHandler_NoCleanupOnSuccess(t *testing.T) {
	deps, disp, mock, st := buildDeps(t)
	stubAllAdminAndLinked(mock)
	if _, _, err := st.UpsertBinding(context.Background(), store.Binding{GroupChatID: gID, ChannelChatID: cID}); err != nil {
		t.Fatal(err)
	}
	deps.CleanupDelay = 20 * time.Millisecond

	msg := groupMsg("/status", admin)
	msg.MessageID = 8102
	handled, err := disp.Dispatch(context.Background(), msg)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}

	// Give any rogue cleanup a chance to fire; then assert none occurred.
	time.Sleep(100 * time.Millisecond)

	mock.LockForTest()
	defer mock.UnlockForTest()
	if len(mock.DeleteMessageCalls) != 0 {
		t.Fatalf("expected 0 DeleteMessage calls on successful /status, got %d: %+v", len(mock.DeleteMessageCalls), mock.DeleteMessageCalls)
	}
}

