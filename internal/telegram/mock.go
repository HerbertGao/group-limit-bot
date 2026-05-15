package telegram

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MockClient records calls and delegates to configurable stub functions.
type MockClient struct {
	MeUser User

	// Stubs — left nil means the method is unexpected (returns error).
	GetChatMemberFn          func(ctx context.Context, chatID, userID int64) (Status, error)
	GetChatMemberCanDeleteFn func(ctx context.Context, chatID, userID int64) (bool, error)
	DeleteMessageFn          func(ctx context.Context, chatID int64, messageID int) error
	SendMessageFn            func(ctx context.Context, chatID int64, text string, markdownV2 bool) (int, error)
	GetChatFn                func(ctx context.Context, chatID int64) (*ChatInfo, error)
	ResolveUsernameFn        func(ctx context.Context, username string) (*ChatInfo, error)
	RestrictChatMemberFn     func(ctx context.Context, chatID, userID int64, until time.Time) error
	BanChatMemberFn          func(ctx context.Context, chatID, userID int64) error
	DeleteWebhookFn          func(ctx context.Context) error

	mu sync.Mutex

	GetChatMemberCalls          []struct{ ChatID, UserID int64 }
	GetChatMemberCanDeleteCalls []struct{ ChatID, UserID int64 }
	DeleteMessageCalls          []struct {
		ChatID    int64
		MessageID int
	}
	SendMessageCalls []struct {
		ChatID     int64
		Text       string
		MarkdownV2 bool
	}
	GetChatCalls         []int64
	ResolveUsernameCalls []string
	RestrictCalls        []struct {
		ChatID, UserID int64
	}
	BanCalls []struct {
		ChatID, UserID int64
	}
	DeleteWebhookCalls int
}

func NewMockClient(me User) *MockClient {
	return &MockClient{MeUser: me}
}

// LockForTest / UnlockForTest expose the internal mutex so tests can safely
// inspect call-log slices populated by background goroutines.
func (m *MockClient) LockForTest()   { m.mu.Lock() }
func (m *MockClient) UnlockForTest() { m.mu.Unlock() }

func (m *MockClient) Me() User { return m.MeUser }

func (m *MockClient) GetChatMember(ctx context.Context, chatID, userID int64) (Status, error) {
	m.mu.Lock()
	m.GetChatMemberCalls = append(m.GetChatMemberCalls, struct{ ChatID, UserID int64 }{chatID, userID})
	m.mu.Unlock()
	if m.GetChatMemberFn == nil {
		return StatusUnknown, fmt.Errorf("mock: GetChatMember not stubbed")
	}
	return m.GetChatMemberFn(ctx, chatID, userID)
}

func (m *MockClient) GetChatMemberCanDelete(ctx context.Context, chatID, userID int64) (bool, error) {
	m.mu.Lock()
	m.GetChatMemberCanDeleteCalls = append(m.GetChatMemberCanDeleteCalls, struct{ ChatID, UserID int64 }{chatID, userID})
	m.mu.Unlock()
	if m.GetChatMemberCanDeleteFn == nil {
		return false, fmt.Errorf("mock: GetChatMemberCanDelete not stubbed")
	}
	return m.GetChatMemberCanDeleteFn(ctx, chatID, userID)
}

func (m *MockClient) DeleteMessage(ctx context.Context, chatID int64, messageID int) error {
	m.mu.Lock()
	m.DeleteMessageCalls = append(m.DeleteMessageCalls, struct {
		ChatID    int64
		MessageID int
	}{chatID, messageID})
	m.mu.Unlock()
	if m.DeleteMessageFn == nil {
		return nil
	}
	return m.DeleteMessageFn(ctx, chatID, messageID)
}

func (m *MockClient) SendMessage(ctx context.Context, chatID int64, text string, markdownV2 bool) (int, error) {
	m.mu.Lock()
	m.SendMessageCalls = append(m.SendMessageCalls, struct {
		ChatID     int64
		Text       string
		MarkdownV2 bool
	}{chatID, text, markdownV2})
	m.mu.Unlock()
	if m.SendMessageFn == nil {
		return 0, nil
	}
	return m.SendMessageFn(ctx, chatID, text, markdownV2)
}

func (m *MockClient) GetChat(ctx context.Context, chatID int64) (*ChatInfo, error) {
	m.mu.Lock()
	m.GetChatCalls = append(m.GetChatCalls, chatID)
	m.mu.Unlock()
	if m.GetChatFn == nil {
		return nil, fmt.Errorf("mock: GetChat not stubbed")
	}
	return m.GetChatFn(ctx, chatID)
}

func (m *MockClient) ResolveUsername(ctx context.Context, username string) (*ChatInfo, error) {
	m.mu.Lock()
	m.ResolveUsernameCalls = append(m.ResolveUsernameCalls, username)
	m.mu.Unlock()
	if m.ResolveUsernameFn == nil {
		return nil, fmt.Errorf("mock: ResolveUsername not stubbed")
	}
	return m.ResolveUsernameFn(ctx, username)
}

func (m *MockClient) RestrictChatMember(ctx context.Context, chatID, userID int64, until time.Time) error {
	m.mu.Lock()
	m.RestrictCalls = append(m.RestrictCalls, struct{ ChatID, UserID int64 }{chatID, userID})
	m.mu.Unlock()
	if m.RestrictChatMemberFn == nil {
		return nil
	}
	return m.RestrictChatMemberFn(ctx, chatID, userID, until)
}

func (m *MockClient) BanChatMember(ctx context.Context, chatID, userID int64) error {
	m.mu.Lock()
	m.BanCalls = append(m.BanCalls, struct{ ChatID, UserID int64 }{chatID, userID})
	m.mu.Unlock()
	if m.BanChatMemberFn == nil {
		return nil
	}
	return m.BanChatMemberFn(ctx, chatID, userID)
}

func (m *MockClient) DeleteWebhook(ctx context.Context) error {
	m.mu.Lock()
	m.DeleteWebhookCalls++
	m.mu.Unlock()
	if m.DeleteWebhookFn == nil {
		return nil
	}
	return m.DeleteWebhookFn(ctx)
}
