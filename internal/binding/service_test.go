package binding

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/herbertgao/group-limit-bot/internal/store"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

const (
	groupID    int64 = -1001
	channelID  int64 = -2001
	botID      int64 = 7777
	adminID    int64 = 100
	nonAdminID int64 = 200
)

func newServiceWithMock(t *testing.T) (*Service, *telegram.MockClient) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mock := telegram.NewMockClient(telegram.User{ID: botID, IsBot: true})
	svc := New(st, mock)
	return svc, mock
}

func groupChatInfo() *telegram.ChatInfo {
	return &telegram.ChatInfo{ID: groupID, Type: "supergroup", Title: "讨论群"}
}

func TestBind_Success(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		switch {
		case chatID == groupID && userID == adminID:
			return telegram.StatusCreator, nil
		case chatID == channelID && userID == botID:
			return telegram.StatusAdministrator, nil
		}
		t.Fatalf("unexpected GetChatMember(%d, %d)", chatID, userID)
		return telegram.StatusUnknown, nil
	}
	mock.GetChatMemberCanDeleteFn = func(ctx context.Context, chatID, userID int64) (bool, error) {
		return true, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		switch chatID {
		case groupID:
			return &telegram.ChatInfo{ID: groupID, Type: "supergroup", Title: "讨论群", LinkedChatID: channelID}, nil
		case channelID:
			return &telegram.ChatInfo{ID: channelID, Type: "channel", Title: "主频道", Username: "main"}, nil
		}
		t.Fatalf("unexpected GetChat(%d)", chatID)
		return nil, nil
	}

	res, err := svc.Bind(context.Background(), groupChatInfo(), adminID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.WasCreated {
		t.Error("expected WasCreated=true on first bind")
	}
	if res.Binding.ChannelChatID != channelID {
		t.Errorf("ChannelChatID = %d", res.Binding.ChannelChatID)
	}
	if res.ChannelTitle != "主频道" {
		t.Errorf("ChannelTitle = %q", res.ChannelTitle)
	}
}

func TestBind_RejectsNonCreator(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusAdministrator, nil
	}
	_, err := svc.Bind(context.Background(), groupChatInfo(), nonAdminID)
	if !errors.Is(err, ErrCallerNotAdmin) {
		t.Errorf("expected ErrCallerNotAdmin, got %v", err)
	}
}

func TestBind_RejectsGroupAdminButNotCreator(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		switch {
		case chatID == groupID && userID == adminID:
			return telegram.StatusAdministrator, nil
		case chatID == channelID && userID == botID:
			return telegram.StatusAdministrator, nil
		}
		return telegram.StatusUnknown, nil
	}
	mock.GetChatMemberCanDeleteFn = func(ctx context.Context, chatID, userID int64) (bool, error) {
		return true, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		if chatID == groupID {
			return &telegram.ChatInfo{ID: groupID, Type: "supergroup", Title: "讨论群", LinkedChatID: channelID}, nil
		}
		return &telegram.ChatInfo{ID: channelID, Type: "channel", Title: "主频道"}, nil
	}
	_, err := svc.Bind(context.Background(), groupChatInfo(), adminID)
	if !errors.Is(err, ErrCallerNotAdmin) {
		t.Errorf("expected ErrCallerNotAdmin (creator-only), got %v", err)
	}
}

func TestBind_RejectsNoLinkedChannel(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusCreator, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		return &telegram.ChatInfo{ID: groupID, Type: "supergroup", Title: "孤群"}, nil
	}
	_, err := svc.Bind(context.Background(), groupChatInfo(), adminID)
	if !errors.Is(err, ErrNoLinkedChannel) {
		t.Errorf("expected ErrNoLinkedChannel, got %v", err)
	}
}

func TestBind_RejectsWhenBotNotChannelAdmin(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		switch {
		case chatID == groupID && userID == adminID:
			return telegram.StatusCreator, nil
		case chatID == channelID && userID == botID:
			return telegram.StatusMember, nil // not admin
		}
		return telegram.StatusUnknown, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		return &telegram.ChatInfo{ID: groupID, Type: "supergroup", Title: "讨论群", LinkedChatID: channelID}, nil
	}
	_, err := svc.Bind(context.Background(), groupChatInfo(), adminID)
	if !errors.Is(err, ErrBotNotChannelAdmin) {
		t.Errorf("expected ErrBotNotChannelAdmin, got %v", err)
	}
}

func TestBind_RejectsWhenBotCannotDeleteInGroup(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		switch {
		case chatID == groupID && userID == adminID:
			return telegram.StatusCreator, nil
		case chatID == channelID && userID == botID:
			return telegram.StatusAdministrator, nil
		}
		return telegram.StatusUnknown, nil
	}
	mock.GetChatMemberCanDeleteFn = func(ctx context.Context, chatID, userID int64) (bool, error) {
		if chatID == groupID && userID == botID {
			return false, nil
		}
		return true, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		switch chatID {
		case groupID:
			return &telegram.ChatInfo{ID: groupID, Type: "supergroup", Title: "讨论群", LinkedChatID: channelID}, nil
		case channelID:
			return &telegram.ChatInfo{ID: channelID, Type: "channel", Title: "主频道"}, nil
		}
		return nil, nil
	}

	_, err := svc.Bind(context.Background(), groupChatInfo(), adminID)
	if !errors.Is(err, ErrBotCannotModerateGroup) {
		t.Errorf("expected ErrBotCannotModerateGroup, got %v", err)
	}
}

func TestBind_OverwritesExisting(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		if userID == adminID {
			return telegram.StatusCreator, nil
		}
		return telegram.StatusAdministrator, nil
	}
	mock.GetChatMemberCanDeleteFn = func(ctx context.Context, chatID, userID int64) (bool, error) {
		return true, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		if chatID == groupID {
			return &telegram.ChatInfo{ID: groupID, Type: "supergroup", Title: "讨论群", LinkedChatID: channelID}, nil
		}
		return &telegram.ChatInfo{ID: channelID, Type: "channel", Title: "频道"}, nil
	}

	first, err := svc.Bind(context.Background(), groupChatInfo(), adminID)
	if err != nil {
		t.Fatal(err)
	}
	if !first.WasCreated {
		t.Error("first bind should be created")
	}
	if first.ChannelChanged {
		t.Error("first bind should not report channel changed")
	}
	second, err := svc.Bind(context.Background(), groupChatInfo(), adminID)
	if err != nil {
		t.Fatal(err)
	}
	if second.WasCreated {
		t.Error("second bind should be update, not create")
	}
	if second.ChannelChanged {
		t.Error("second bind kept same channel, ChannelChanged should be false")
	}
}

func TestBind_ChannelChangeFlagged(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		if userID == adminID {
			return telegram.StatusCreator, nil
		}
		return telegram.StatusAdministrator, nil
	}
	mock.GetChatMemberCanDeleteFn = func(ctx context.Context, chatID, userID int64) (bool, error) {
		return true, nil
	}
	// First bind: linked channel = channelID.
	firstLinked := channelID
	secondLinked := int64(-3001)
	linked := &firstLinked
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		if chatID == groupID {
			return &telegram.ChatInfo{ID: groupID, Type: "supergroup", Title: "讨论群", LinkedChatID: *linked}, nil
		}
		return &telegram.ChatInfo{ID: chatID, Type: "channel", Title: "频道"}, nil
	}

	first, err := svc.Bind(context.Background(), groupChatInfo(), adminID)
	if err != nil {
		t.Fatal(err)
	}
	if !first.WasCreated || first.ChannelChanged {
		t.Fatalf("unexpected first: %+v", first)
	}

	// Swap linked channel so second bind sees a different linked_chat_id.
	linked = &secondLinked
	second, err := svc.Bind(context.Background(), groupChatInfo(), adminID)
	if err != nil {
		t.Fatal(err)
	}
	if second.WasCreated {
		t.Error("second bind should update, not create")
	}
	if !second.ChannelChanged {
		t.Error("channel switched, ChannelChanged should be true")
	}
	if second.Binding.ChannelChatID != secondLinked {
		t.Errorf("expected persisted channel = %d, got %d", secondLinked, second.Binding.ChannelChatID)
	}
}

func TestUnbind_Success(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		if userID == adminID {
			return telegram.StatusCreator, nil
		}
		return telegram.StatusAdministrator, nil
	}
	mock.GetChatMemberCanDeleteFn = func(ctx context.Context, chatID, userID int64) (bool, error) {
		return true, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		if chatID == groupID {
			return &telegram.ChatInfo{ID: groupID, Type: "supergroup", LinkedChatID: channelID}, nil
		}
		return &telegram.ChatInfo{ID: channelID, Type: "channel", Title: "频道"}, nil
	}
	if _, err := svc.Bind(context.Background(), groupChatInfo(), adminID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Unbind(context.Background(), groupID, adminID); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	// A second unbind should return ErrNotBound.
	if err := svc.Unbind(context.Background(), groupID, adminID); !errors.Is(err, ErrNotBound) {
		t.Errorf("expected ErrNotBound, got %v", err)
	}
}

func TestUnbind_RejectsNonCreator(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		return telegram.StatusAdministrator, nil
	}
	err := svc.Unbind(context.Background(), groupID, nonAdminID)
	if !errors.Is(err, ErrCallerNotAdmin) {
		t.Errorf("expected ErrCallerNotAdmin, got %v", err)
	}
}

func TestBind_APIFailureOnChannelCheckReportsBotNotChannelAdmin(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		switch {
		case chatID == groupID && userID == adminID:
			return telegram.StatusCreator, nil
		case chatID == channelID && userID == botID:
			return telegram.StatusUnknown, errors.New("forbidden: bot not a member")
		}
		return telegram.StatusUnknown, nil
	}
	mock.GetChatMemberCanDeleteFn = func(ctx context.Context, chatID, userID int64) (bool, error) {
		return true, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		switch chatID {
		case groupID:
			return &telegram.ChatInfo{ID: groupID, Type: "supergroup", Title: "讨论群", LinkedChatID: channelID}, nil
		case channelID:
			return &telegram.ChatInfo{ID: channelID, Type: "channel", Title: "主频道"}, nil
		}
		return nil, nil
	}

	_, err := svc.Bind(context.Background(), groupChatInfo(), adminID)
	if !errors.Is(err, ErrBotNotChannelAdmin) {
		t.Errorf("expected ErrBotNotChannelAdmin on non-transient API error, got %v", err)
	}
}

func TestBind_APIFailureOnGroupCheckReportsCannotModerate(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		switch {
		case chatID == groupID && userID == adminID:
			return telegram.StatusCreator, nil
		case chatID == channelID && userID == botID:
			return telegram.StatusAdministrator, nil
		}
		return telegram.StatusUnknown, nil
	}
	mock.GetChatMemberCanDeleteFn = func(ctx context.Context, chatID, userID int64) (bool, error) {
		if chatID == groupID && userID == botID {
			return false, errors.New("bad request: chat_admin_required")
		}
		return true, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		switch chatID {
		case groupID:
			return &telegram.ChatInfo{ID: groupID, Type: "supergroup", Title: "讨论群", LinkedChatID: channelID}, nil
		case channelID:
			return &telegram.ChatInfo{ID: channelID, Type: "channel", Title: "主频道"}, nil
		}
		return nil, nil
	}

	_, err := svc.Bind(context.Background(), groupChatInfo(), adminID)
	if !errors.Is(err, ErrBotCannotModerateGroup) {
		t.Errorf("expected ErrBotCannotModerateGroup on non-transient API error, got %v", err)
	}
}

func TestBind_TransientChannelCheckErrorBubbles(t *testing.T) {
	svc, mock := newServiceWithMock(t)
	mock.GetChatMemberFn = func(ctx context.Context, chatID, userID int64) (telegram.Status, error) {
		switch {
		case chatID == groupID && userID == adminID:
			return telegram.StatusCreator, nil
		case chatID == channelID && userID == botID:
			return telegram.StatusUnknown, &telegram.RateLimitError{ChatID: channelID, RetryAfter: 5 * time.Second}
		}
		return telegram.StatusUnknown, nil
	}
	mock.GetChatFn = func(ctx context.Context, chatID int64) (*telegram.ChatInfo, error) {
		switch chatID {
		case groupID:
			return &telegram.ChatInfo{ID: groupID, Type: "supergroup", Title: "讨论群", LinkedChatID: channelID}, nil
		case channelID:
			return &telegram.ChatInfo{ID: channelID, Type: "channel", Title: "主频道"}, nil
		}
		return nil, nil
	}

	_, err := svc.Bind(context.Background(), groupChatInfo(), adminID)
	if err == nil {
		t.Fatal("expected transient error to bubble, got nil")
	}
	if errors.Is(err, ErrBotNotChannelAdmin) {
		t.Errorf("transient error must not be mapped to ErrBotNotChannelAdmin: %v", err)
	}
	if !strings.Contains(err.Error(), "check bot admin in channel") {
		t.Errorf("expected wrapped message containing 'check bot admin in channel', got %v", err)
	}
}
