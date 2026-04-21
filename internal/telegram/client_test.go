package telegram

import (
	"context"
	"errors"
	"log/slog"
	"io"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"golang.org/x/time/rate"
)

// newTestClient builds a TelegoClient without touching the network.
// Only the fields exercised by acquire() are populated.
func newTestClient() *TelegoClient {
	return &TelegoClient{
		me:        User{ID: 1, IsBot: true},
		limiter:   rate.NewLimiter(rate.Inf, 1),
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		chatPause: make(map[int64]time.Time),
	}
}

func TestAcquire_WaitsForChatPauseInsteadOfFailingFast(t *testing.T) {
	c := newTestClient()
	const chatID int64 = 42
	c.setPause(chatID, 200*time.Millisecond)

	start := time.Now()
	err := c.acquire(context.Background(), chatID)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if elapsed < 180*time.Millisecond {
		t.Errorf("expected acquire to block at least ~180ms, got %s", elapsed)
	}
}

func TestSetPause_KeepsLongerDeadline(t *testing.T) {
	c := newTestClient()
	const chatID int64 = 42

	c.setPause(chatID, 300*time.Millisecond)
	c.mu.Lock()
	pause1 := c.chatPause[chatID]
	c.mu.Unlock()

	c.setPause(chatID, 50*time.Millisecond)
	c.mu.Lock()
	pause2 := c.chatPause[chatID]
	c.mu.Unlock()
	if !pause2.Equal(pause1) {
		t.Fatalf("shorter pause overwrote longer one: pause1=%v pause2=%v", pause1, pause2)
	}

	time.Sleep(80 * time.Millisecond)
	c.mu.Lock()
	remaining := time.Until(c.chatPause[chatID])
	c.mu.Unlock()
	if remaining < 200*time.Millisecond {
		t.Errorf("expected remaining pause >= 200ms, got %s", remaining)
	}

	before := time.Now()
	c.setPause(chatID, 500*time.Millisecond)
	c.mu.Lock()
	pause3 := c.chatPause[chatID]
	c.mu.Unlock()
	if pause3.Sub(before) < 480*time.Millisecond {
		t.Errorf("expected extension to ~now+500ms, got delta=%s", pause3.Sub(before))
	}
}

func TestStatusFromTelegoMember_RestrictedRespectsIsMember(t *testing.T) {
	cases := []struct {
		name   string
		member telego.ChatMember
		want   Status
	}{
		{"restricted still member", &telego.ChatMemberRestricted{IsMember: true}, StatusRestricted},
		{"restricted left chat", &telego.ChatMemberRestricted{IsMember: false}, StatusLeft},
		{"member", &telego.ChatMemberMember{}, StatusMember},
		{"left", &telego.ChatMemberLeft{}, StatusLeft},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusFromTelegoMember(tc.member); got != tc.want {
				t.Errorf("statusFromTelegoMember: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAcquire_CancelsOnContext(t *testing.T) {
	c := newTestClient()
	const chatID int64 = 42
	c.setPause(chatID, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.acquire(ctx, chatID)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context error, got %v", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("expected cancel within ~200ms, took %s", elapsed)
	}
}
