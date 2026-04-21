package commands

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/mymmrac/telego"
)

// Handler handles a single command invocation.
// args is the text after the command token (may be empty).
type Handler func(ctx context.Context, msg *telego.Message, args string) error

// Dispatcher routes `/command[@botusername]` messages to registered handlers.
type Dispatcher struct {
	botUsername string
	log         *slog.Logger

	mu       sync.RWMutex
	handlers map[string]Handler
}

func NewDispatcher(botUsername string, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		botUsername: strings.ToLower(botUsername),
		log:         log,
		handlers:    make(map[string]Handler),
	}
}

// Register binds cmd (e.g. "bind") to h. cmd is normalized to lowercase and
// may be provided with or without a leading slash.
func (d *Dispatcher) Register(cmd string, h Handler) {
	cmd = strings.ToLower(strings.TrimPrefix(cmd, "/"))
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[cmd] = h
}

// isValidBotUsername reports whether s is a plausible Telegram bot username
// (5-32 chars, [A-Za-z0-9_], starting with a letter). We don't enforce the
// `bot` suffix because some deployments use custom usernames that end elsewhere.
func isValidBotUsername(s string) bool {
	if len(s) < 5 || len(s) > 32 {
		return false
	}
	first := s[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// parse extracts the command keyword and the remainder.
// Returns (cmd, args, matchesThisBot, ok).
//
//	matchesThisBot is true when the message was either `/x` (no @ suffix) or
//	`/x@<our username>`. Messages `/x@other_bot` → matchesThisBot == false.
func (d *Dispatcher) parse(text string) (cmd string, args string, matchesThisBot bool, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false, false
	}
	body := text[1:]
	// Split at first whitespace for args.
	var head string
	if idx := strings.IndexAny(body, " \t\n\r"); idx >= 0 {
		head = body[:idx]
		args = strings.TrimSpace(body[idx+1:])
	} else {
		head = body
	}
	if head == "" {
		return "", "", false, false
	}
	// Extract optional @bot_username suffix.
	matchesThisBot = true
	if atIdx := strings.Index(head, "@"); atIdx >= 0 {
		name := strings.ToLower(head[atIdx+1:])
		head = head[:atIdx]
		if head == "" {
			return "", "", false, false
		}
		if !isValidBotUsername(name) {
			return "", "", false, false
		}
		matchesThisBot = (name == d.botUsername)
	}
	head = strings.ToLower(head)
	return head, args, matchesThisBot, true
}

// Matches reports whether text is a PURE command invocation to this bot
// whose keyword is registered. A pure command has no trailing text after the
// command token (optionally followed by `@bot_username`). Used by the gate
// to short-circuit moderation only for legitimate bare commands; anything
// with trailing user text (e.g. `/status buy my scam`) returns false and
// must go through the full gating pipeline.
func (d *Dispatcher) Matches(text string) bool {
	cmd, args, toThisBot, ok := d.parse(text)
	if !ok || !toThisBot || args != "" {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, reg := d.handlers[cmd]
	return reg
}

// IsPureCommand reports whether text is a pure bot-command invocation:
// either `/cmd` / `/cmd@this_bot` of a REGISTERED command, or
// `/cmd@any_other_bot_username` (a command to another bot that we should not
// moderate). Non-pure invocations (with trailing text) return false.
//
// Used by the gate to short-circuit command-shaped messages that must not
// be deleted as ordinary user text.
func (d *Dispatcher) IsPureCommand(text string) bool {
	cmd, args, toThisBot, ok := d.parse(text)
	if !ok || args != "" {
		return false
	}
	if !toThisBot {
		// `/cmd@other_bot` — a pure command to a different bot. Let that bot handle it.
		return true
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, reg := d.handlers[cmd]
	return reg
}

// Dispatch routes the message to the appropriate handler. STRICT: only pure
// command invocations (no trailing text) are dispatched. Messages like
// `/cmd extra` fall through (handled=false) and are moderated by the gating
// pipeline as regular user text. Only `msg.Text` is inspected; captions on
// media messages are never treated as commands.
func (d *Dispatcher) Dispatch(ctx context.Context, msg *telego.Message) (handled bool, err error) {
	cmd, args, toThisBot, ok := d.parse(msg.Text)
	if !ok || !toThisBot || args != "" {
		return false, nil
	}
	d.mu.RLock()
	h, reg := d.handlers[cmd]
	d.mu.RUnlock()
	if !reg {
		return false, nil
	}
	return true, h(ctx, msg, args)
}
