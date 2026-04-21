package commands

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/mymmrac/telego"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestDispatch_MatchesPlainCommand(t *testing.T) {
	d := NewDispatcher("my_bot", quietLogger())
	called := false
	d.Register("ping", func(ctx context.Context, msg *telego.Message, args string) error {
		called = true
		if args != "" {
			t.Errorf("args = %q (Dispatch is strict; pure commands carry no args)", args)
		}
		return nil
	})
	msg := &telego.Message{Text: "/ping"}
	handled, err := d.Dispatch(context.Background(), msg)
	if err != nil || !handled || !called {
		t.Errorf("handled=%v err=%v called=%v", handled, err, called)
	}
}

func TestDispatch_MatchesWithBotUsernameCaseInsensitive(t *testing.T) {
	d := NewDispatcher("My_Guardian_Bot", quietLogger())
	called := false
	d.Register("status", func(ctx context.Context, msg *telego.Message, args string) error {
		called = true
		return nil
	})
	msg := &telego.Message{Text: "/status@my_guardian_BOT"}
	handled, err := d.Dispatch(context.Background(), msg)
	if err != nil || !handled || !called {
		t.Errorf("handled=%v err=%v called=%v", handled, err, called)
	}
}

func TestDispatch_IgnoresOtherBot(t *testing.T) {
	d := NewDispatcher("my_bot", quietLogger())
	d.Register("status", func(ctx context.Context, msg *telego.Message, args string) error {
		t.Fatal("should not be called")
		return nil
	})
	msg := &telego.Message{Text: "/status@other_bot"}
	handled, err := d.Dispatch(context.Background(), msg)
	if err != nil || handled {
		t.Errorf("handled=%v err=%v (should be false)", handled, err)
	}
}

func TestDispatch_IgnoresUnregistered(t *testing.T) {
	d := NewDispatcher("my_bot", quietLogger())
	msg := &telego.Message{Text: "/unknown"}
	handled, err := d.Dispatch(context.Background(), msg)
	if err != nil || handled {
		t.Errorf("handled=%v err=%v (should be false)", handled, err)
	}
}

func TestMatches(t *testing.T) {
	d := NewDispatcher("my_bot", quietLogger())
	d.Register("bind", func(ctx context.Context, msg *telego.Message, args string) error { return nil })
	if !d.Matches("/bind") {
		t.Error("should match /bind")
	}
	if !d.Matches("/bind@MY_BOT") {
		t.Error("should match case-insensitively")
	}
	if d.Matches("/bind@other_bot") {
		t.Error("should not match other bot")
	}
	if d.Matches("/unknown") {
		t.Error("should not match unregistered")
	}
	if d.Matches("hello /bind") {
		t.Error("should require leading slash")
	}
	if d.Matches("") {
		t.Error("empty text should not match")
	}
}

// Matches must be STRICT: only pure `/cmd` or `/cmd@bot` invocations short-circuit
// the gate. Any trailing user text makes the message subject to moderation.
func TestMatches_StrictOnPureCommand(t *testing.T) {
	d := NewDispatcher("my_bot", quietLogger())
	d.Register("status", func(ctx context.Context, msg *telego.Message, args string) error { return nil })

	if !d.Matches("/status") {
		t.Error("pure /status should match")
	}
	if !d.Matches("/status@my_bot") {
		t.Error("pure /status@my_bot should match")
	}
	if d.Matches("/status anything") {
		t.Error("/status with trailing text must NOT match")
	}
	if d.Matches("/status@my_bot extra") {
		t.Error("/status@my_bot with trailing text must NOT match")
	}
}

// Dispatch must be STRICT: only pure `/cmd` or `/cmd@bot` invocations are routed
// to handlers. Any trailing user text (e.g. `/ping extra`) must fall through
// (handled=false) so the gating pipeline moderates it as a normal message.
func TestDispatch_StrictOnPureCommand(t *testing.T) {
	d := NewDispatcher("my_bot", quietLogger())
	d.Register("ping", func(ctx context.Context, msg *telego.Message, args string) error {
		t.Fatalf("handler must not be called for non-pure command, args=%q", args)
		return nil
	})

	handled, err := d.Dispatch(context.Background(), &telego.Message{Text: "/ping extra"})
	if err != nil || handled {
		t.Errorf("/ping extra: handled=%v err=%v (want handled=false)", handled, err)
	}

	handled, err = d.Dispatch(context.Background(), &telego.Message{Text: "/ping@my_bot anything"})
	if err != nil || handled {
		t.Errorf("/ping@my_bot anything: handled=%v err=%v (want handled=false)", handled, err)
	}
}

func TestIsPureCommand(t *testing.T) {
	d := NewDispatcher("my_bot", quietLogger())
	d.Register("ping", func(ctx context.Context, msg *telego.Message, args string) error { return nil })

	cases := []struct {
		text string
		want bool
	}{
		{"/ping", true},
		{"/ping@my_bot", true},
		{"/ping@OTHER_Bot", true}, // other bot — case-insensitive; ours is "my_bot"
		{"/ping@my_bot extra", false},
		{"/unknown", false},
		{"/unknown@my_bot", false},
		{"/unknown@other_bot", true},
		{"hello", false},
		{"", false},
	}
	for _, c := range cases {
		if got := d.IsPureCommand(c.text); got != c.want {
			t.Errorf("IsPureCommand(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestIsPureCommand_RejectsMalformedAt(t *testing.T) {
	d := NewDispatcher("my_bot", quietLogger())
	d.Register("status", func(ctx context.Context, msg *telego.Message, args string) error { return nil })

	cases := []struct {
		text string
		want bool
	}{
		{"/status@", false},
		{"/spam@", false},
		{"/@other_bot", false},
		{"/status@x", false},
		{"/status@_invalid", false},
		{"/status@0startsdigit", false},
		{"/status@valid_bot", true},
	}
	for _, c := range cases {
		if got := d.IsPureCommand(c.text); got != c.want {
			t.Errorf("IsPureCommand(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestDispatch_IgnoresMalformedAt(t *testing.T) {
	d := NewDispatcher("my_bot", quietLogger())
	d.Register("status", func(ctx context.Context, msg *telego.Message, args string) error {
		t.Fatal("handler must not be called for malformed @ suffix")
		return nil
	})
	handled, err := d.Dispatch(context.Background(), &telego.Message{Text: "/status@"})
	if err != nil || handled {
		t.Errorf("/status@: handled=%v err=%v (want handled=false)", handled, err)
	}
}

// Leading whitespace must disqualify a message from being treated as a command.
// Otherwise non-members could bypass moderation by prefixing `/cmd` with a space,
// tab, or newline.
func TestParse_RejectsLeadingWhitespace(t *testing.T) {
	d := NewDispatcher("my_bot", quietLogger())
	d.Register("ping", func(ctx context.Context, msg *telego.Message, args string) error {
		t.Fatalf("handler must not be called for leading-whitespace command, args=%q", args)
		return nil
	})

	for _, text := range []string{" /ping", "\n/ping", "\t/ping"} {
		handled, err := d.Dispatch(context.Background(), &telego.Message{Text: text})
		if err != nil || handled {
			t.Errorf("Dispatch(%q): handled=%v err=%v (want handled=false)", text, handled, err)
		}
		if d.Matches(text) {
			t.Errorf("Matches(%q) = true, want false", text)
		}
		if d.IsPureCommand(text) {
			t.Errorf("IsPureCommand(%q) = true, want false", text)
		}
	}

	if !d.Matches("/ping") {
		t.Error("Matches(\"/ping\") must still be true")
	}
	if !d.Matches("/ping ") {
		t.Error("Matches(\"/ping \") (trailing space) must still be true (pure — args empty after trim)")
	}
}

// Dispatch must only inspect msg.Text. A caption on a media message — even one
// that looks exactly like a registered command — must not be routed to any handler.
func TestDispatch_IgnoresCaptionOnMedia(t *testing.T) {
	d := NewDispatcher("my_bot", quietLogger())
	d.Register("status", func(ctx context.Context, msg *telego.Message, args string) error {
		t.Fatal("handler must not be called for caption-only command")
		return nil
	})
	msg := &telego.Message{Text: "", Caption: "/status"}
	handled, err := d.Dispatch(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if handled {
		t.Error("caption must not trigger dispatch")
	}
}
