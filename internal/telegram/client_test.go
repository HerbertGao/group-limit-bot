package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	"golang.org/x/time/rate"
)

// TestIsTransient guards the regression that motivated this classifier: a
// fasthttp transport timeout is NOT context.DeadlineExceeded, so the old logic
// treated every network hiccup as a definitive permission failure (selfcheck
// spammed every group; /bind wrongly reported "bot is not admin").
func TestIsTransient(t *testing.T) {
	transient := []struct {
		name string
		err  error
	}{
		{"rate limit", &RateLimitError{ChatID: 1, RetryAfter: time.Second}},
		{"context deadline", context.DeadlineExceeded},
		{"context canceled", context.Canceled},
		{"fasthttp timeout", errors.New("telego: getChatMember: internal execution: request call: fasthttp do request: timeout")},
		{"connection reset", errors.New("fasthttp do request: connection reset by peer")},
		// telego's FastHTTPCaller surfaces HTTP 5xx as this plain string, never a *telegoapi.Error.
		{"server 5xx", errors.New("internal server error: 502")},
		{"wrapped 429 api error", fmt.Errorf("call: %w", &telegoapi.Error{ErrorCode: 429, Description: "Too Many Requests"})},
	}
	for _, tc := range transient {
		t.Run(tc.name, func(t *testing.T) {
			if !IsTransient(tc.err) {
				t.Errorf("expected %q to be transient", tc.name)
			}
		})
	}

	definitive := []struct {
		name string
		err  error
	}{
		{"chat not found", &telegoapi.Error{ErrorCode: 400, Description: "Bad Request: chat not found"}},
		{"bot kicked", fmt.Errorf("call: %w", &telegoapi.Error{ErrorCode: 403, Description: "Forbidden: bot was kicked from the supergroup chat"})},
		// A definitive 4xx must stay definitive even when an outer wrapping mentions
		// "timeout" — guards against any future naive string-matching shortcut.
		{"4xx wrapped with timeout-ish text", fmt.Errorf("getChatMember timeout: %w", &telegoapi.Error{ErrorCode: 400, Description: "chat not found"})},
	}
	for _, tc := range definitive {
		t.Run(tc.name, func(t *testing.T) {
			if IsTransient(tc.err) {
				t.Errorf("expected %q to be a definitive (non-transient) signal", tc.name)
			}
		})
	}

	// nil is no error at all, not a transient failure.
	if IsTransient(nil) {
		t.Error("nil error must not be classified transient")
	}
}

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
