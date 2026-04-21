package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	"golang.org/x/time/rate"
)

// GroupAnonymousBotID is the Telegram system user that represents
// "group anonymous admin" sending messages on behalf of the group.
const GroupAnonymousBotID int64 = 1087968824

// Status is the canonical membership state we care about.
type Status int

const (
	StatusUnknown Status = iota
	StatusCreator
	StatusAdministrator
	StatusMember
	StatusRestricted
	StatusLeft
	StatusBanned
)

func (s Status) String() string {
	switch s {
	case StatusCreator:
		return "creator"
	case StatusAdministrator:
		return "administrator"
	case StatusMember:
		return "member"
	case StatusRestricted:
		return "restricted"
	case StatusLeft:
		return "left"
	case StatusBanned:
		return "kicked"
	}
	return "unknown"
}

// InChat reports whether the user is currently in the chat in any capacity
// (owner, admin, regular member, or restricted-but-present).
func (s Status) InChat() bool {
	return s == StatusCreator || s == StatusAdministrator || s == StatusMember || s == StatusRestricted
}

// IsAdmin reports whether the user is an owner or administrator.
func (s Status) IsAdmin() bool {
	return s == StatusCreator || s == StatusAdministrator
}

// IsCreator reports whether the member is the chat's creator (owner).
func (s Status) IsCreator() bool { return s == StatusCreator }

type ChatInfo struct {
	ID           int64
	Type         string
	Title        string
	Username     string
	LinkedChatID int64
}

type User struct {
	ID       int64
	IsBot    bool
	Username string
}

// Client is the thin Telegram surface the app depends on.
type Client interface {
	Me() User
	GetChatMember(ctx context.Context, chatID, userID int64) (Status, error)
	// GetChatMemberCanDelete reports whether userID has permission to delete messages
	// in chatID. True for owners (creator) and for administrators whose
	// CanDeleteMessages is true.
	GetChatMemberCanDelete(ctx context.Context, chatID, userID int64) (bool, error)
	DeleteMessage(ctx context.Context, chatID int64, messageID int) error
	SendMessage(ctx context.Context, chatID int64, text string, markdownV2 bool) (int, error)
	GetChat(ctx context.Context, chatID int64) (*ChatInfo, error)
	DeleteWebhook(ctx context.Context) error
}

// RateLimitError is returned when we are actively backing off a chat due to 429.
type RateLimitError struct {
	ChatID     int64
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited for chat %d, retry after %s", e.ChatID, e.RetryAfter)
}

// TelegoClient wraps *telego.Bot with a global token bucket and per-chat 429 backoff.
type TelegoClient struct {
	bot     *telego.Bot
	me      User
	limiter *rate.Limiter
	log     *slog.Logger

	mu        sync.Mutex
	chatPause map[int64]time.Time
}

// NewTelegoClient constructs the client. It also calls GetMe to capture the bot's identity.
func NewTelegoClient(ctx context.Context, token string, log *slog.Logger) (*TelegoClient, error) {
	bot, err := telego.NewBot(token, telego.WithDefaultLogger(false, false))
	if err != nil {
		return nil, fmt.Errorf("telego.NewBot: %w", err)
	}
	me, err := bot.GetMe(ctx)
	if err != nil {
		return nil, fmt.Errorf("getMe: %w", err)
	}
	c := &TelegoClient{
		bot:       bot,
		me:        User{ID: me.ID, IsBot: me.IsBot, Username: me.Username},
		limiter:   rate.NewLimiter(rate.Limit(30), 30),
		log:       log,
		chatPause: make(map[int64]time.Time),
	}
	return c, nil
}

func (c *TelegoClient) Bot() *telego.Bot { return c.bot }

func (c *TelegoClient) Me() User { return c.me }

// setPause extends the per-chat backoff window. If a longer pause is already
// in effect, keep it — concurrent 429 responses must not shorten existing waits.
func (c *TelegoClient) setPause(chatID int64, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline := time.Now().Add(d)
	if existing, ok := c.chatPause[chatID]; ok && existing.After(deadline) {
		return
	}
	c.chatPause[chatID] = deadline
}

// handleAPIError inspects the error and updates per-chat pause on 429.
// Returns the (possibly wrapped) error for the caller.
func (c *TelegoClient) handleAPIError(chatID int64, err error) error {
	if err == nil {
		return nil
	}
	var apiErr *telegoapi.Error
	if errors.As(err, &apiErr) && apiErr.ErrorCode == 429 {
		retry := time.Second
		if apiErr.Parameters != nil && apiErr.Parameters.RetryAfter > 0 {
			retry = time.Duration(apiErr.Parameters.RetryAfter) * time.Second
		}
		c.setPause(chatID, retry)
		if c.log != nil {
			c.log.Warn("telegram 429",
				slog.Int64("chat_id", chatID),
				slog.Duration("retry_after", retry),
				slog.String("description", apiErr.Description),
			)
		}
		return &RateLimitError{ChatID: chatID, RetryAfter: retry}
	}
	return err
}

// acquire blocks until any per-chat 429 backoff window has elapsed, then waits
// for a global rate-limiter token. On ctx cancellation it returns ctx.Err().
// It never returns a RateLimitError locally — that type is reserved for errors
// surfaced from actual 429 responses by handleAPIError.
func (c *TelegoClient) acquire(ctx context.Context, chatID int64) error {
	for {
		c.mu.Lock()
		until, paused := c.chatPause[chatID]
		now := time.Now()
		if paused && !now.Before(until) {
			delete(c.chatPause, chatID)
			paused = false
		}
		c.mu.Unlock()
		if !paused {
			break
		}
		wait := time.Until(until)
		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			// Loop and re-check: another 429 may have extended the pause.
		}
	}
	return c.limiter.Wait(ctx)
}

// withTimeout returns ctx capped at 5s unless ctx already has a shorter deadline.
func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, 5*time.Second)
}

func (c *TelegoClient) GetChatMember(ctx context.Context, chatID, userID int64) (Status, error) {
	// acquire honors ctx.Done(); withTimeout bounds only the API round-trip so long 429 retry_after waits aren't cut short.
	if err := c.acquire(ctx, chatID); err != nil {
		return StatusUnknown, err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	member, err := c.bot.GetChatMember(ctx, &telego.GetChatMemberParams{
		ChatID: telego.ChatID{ID: chatID},
		UserID: userID,
	})
	if err != nil {
		return StatusUnknown, c.handleAPIError(chatID, err)
	}
	return statusFromTelegoMember(member), nil
}

func (c *TelegoClient) GetChatMemberCanDelete(ctx context.Context, chatID, userID int64) (bool, error) {
	if err := c.acquire(ctx, chatID); err != nil {
		return false, err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	member, err := c.bot.GetChatMember(ctx, &telego.GetChatMemberParams{
		ChatID: telego.ChatID{ID: chatID},
		UserID: userID,
	})
	if err != nil {
		return false, c.handleAPIError(chatID, err)
	}
	switch m := member.(type) {
	case *telego.ChatMemberOwner:
		return true, nil
	case *telego.ChatMemberAdministrator:
		if m.CanDeleteMessages {
			return true, nil
		}
		return false, nil
	}
	return false, nil
}

func (c *TelegoClient) DeleteMessage(ctx context.Context, chatID int64, messageID int) error {
	if err := c.acquire(ctx, chatID); err != nil {
		return err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	err := c.bot.DeleteMessage(ctx, &telego.DeleteMessageParams{
		ChatID:    telego.ChatID{ID: chatID},
		MessageID: messageID,
	})
	return c.handleAPIError(chatID, err)
}

func (c *TelegoClient) SendMessage(ctx context.Context, chatID int64, text string, markdownV2 bool) (int, error) {
	if err := c.acquire(ctx, chatID); err != nil {
		return 0, err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	params := &telego.SendMessageParams{
		ChatID: telego.ChatID{ID: chatID},
		Text:   text,
	}
	if markdownV2 {
		params.ParseMode = telego.ModeMarkdownV2
	}
	msg, err := c.bot.SendMessage(ctx, params)
	if err != nil {
		return 0, c.handleAPIError(chatID, err)
	}
	return msg.MessageID, nil
}

func (c *TelegoClient) GetChat(ctx context.Context, chatID int64) (*ChatInfo, error) {
	if err := c.acquire(ctx, chatID); err != nil {
		return nil, err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	full, err := c.bot.GetChat(ctx, &telego.GetChatParams{ChatID: telego.ChatID{ID: chatID}})
	if err != nil {
		return nil, c.handleAPIError(chatID, err)
	}
	return &ChatInfo{
		ID:           full.ID,
		Type:         full.Type,
		Title:        full.Title,
		Username:     full.Username,
		LinkedChatID: full.LinkedChatID,
	}, nil
}

func (c *TelegoClient) DeleteWebhook(ctx context.Context) error {
	if err := c.acquire(ctx, 0); err != nil {
		return err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	err := c.bot.DeleteWebhook(ctx, &telego.DeleteWebhookParams{DropPendingUpdates: false})
	return c.handleAPIError(0, err)
}

// statusFromTelegoMember converts a telego.ChatMember to our Status,
// preserving the ChatMemberRestricted.IsMember bit which the string-based
// MemberStatus() alone would lose.
func statusFromTelegoMember(m telego.ChatMember) Status {
	if r, ok := m.(*telego.ChatMemberRestricted); ok {
		if !r.IsMember {
			return StatusLeft
		}
		return StatusRestricted
	}
	return statusFromTelego(m.MemberStatus())
}

func statusFromTelego(s string) Status {
	switch s {
	case telego.MemberStatusCreator:
		return StatusCreator
	case telego.MemberStatusAdministrator:
		return StatusAdministrator
	case telego.MemberStatusMember:
		return StatusMember
	case telego.MemberStatusRestricted:
		return StatusRestricted
	case telego.MemberStatusLeft:
		return StatusLeft
	case telego.MemberStatusBanned:
		return StatusBanned
	}
	return StatusUnknown
}

// EscapeMarkdownV2 escapes a literal string for safe interpolation into MarkdownV2 text.
// Reference: https://core.telegram.org/bots/api#markdownv2-style
func EscapeMarkdownV2(s string) string {
	const special = "_*[]()~`>#+-=|{}.!\\"
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if strings.ContainsRune(special, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
