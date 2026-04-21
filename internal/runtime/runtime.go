package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/mymmrac/telego"

	"github.com/herbertgao/group-limit-bot/internal/binding"
	"github.com/herbertgao/group-limit-bot/internal/commands"
	"github.com/herbertgao/group-limit-bot/internal/config"
	"github.com/herbertgao/group-limit-bot/internal/gating"
	"github.com/herbertgao/group-limit-bot/internal/logging"
	"github.com/herbertgao/group-limit-bot/internal/metrics"
	"github.com/herbertgao/group-limit-bot/internal/store"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

const updateHandlerConcurrency = 128

// Run assembles the bot and blocks until the process receives SIGINT/SIGTERM.
func Run(ctx context.Context, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	log := logging.New(cfg.LogLevel)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	tg, err := telegram.NewTelegoClient(ctx, cfg.BotToken, log)
	if err != nil {
		return fmt.Errorf("telegram client: %w", err)
	}
	me := tg.Me()
	log.Info("bot identity",
		slog.Int64("user_id", me.ID),
		slog.String("username", me.Username),
	)

	if err := deleteWebhookWithRetry(ctx, tg, log); err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}

	reg := metrics.NewRegistry()
	cache := gating.NewMemberCache(st, cfg.CacheTTL)
	if err := cache.Prime(ctx, time.Now()); err != nil {
		log.Warn("prime cache failed", slog.String("error", err.Error()))
	}
	dedup := gating.NewMediaGroupDedup(60 * time.Second)

	bindSvc := binding.New(st, tg)

	disp := commands.NewDispatcher(me.Username, log)

	botAllow := make(map[int64]bool, len(cfg.BotAllowlist))
	for _, id := range cfg.BotAllowlist {
		botAllow[id] = true
	}
	gate := gating.New(bindSvc, cache, dedup, tg, reg, log, gating.Config{
		TTL:                 cfg.CacheTTL,
		BotAllowlist:        botAllow,
		AllowAnonymousAdmin: cfg.AllowAnonymousAdmin,
		IsCommandText:       disp.IsPureCommand,
	})
	exec := gating.NewExecutor(tg, reg, log)

	deps := &commands.Deps{
		BindSvc:      bindSvc,
		TG:           tg,
		Store:        st,
		Metrics:      reg,
		Cache:        cache,
		Log:          log,
		CleanupDelay: 10 * time.Second,
	}
	deps.Register(disp)

	// Signal handling: cancel pollCtx on SIGINT/SIGTERM.
	rootCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Background housekeeping loops share rootCtx and exit on shutdown.
	var bgWG sync.WaitGroup
	bgWG.Add(2)
	go func() {
		defer bgWG.Done()
		cleanupLoop(rootCtx, st, cache, log)
	}()
	go func() {
		defer bgWG.Done()
		NewSelfCheck(tg, st, log).Run(rootCtx, 5*time.Minute)
	}()

	// Long polling.
	updates, err := tg.Bot().UpdatesViaLongPolling(rootCtx, &telego.GetUpdatesParams{
		Timeout:        30,
		AllowedUpdates: []string{"message", "edited_message", "my_chat_member"},
	})
	if err != nil {
		return fmt.Errorf("start long polling: %w", err)
	}
	log.Info("bot started, polling for updates")

	// Handlers use a context independent of rootCtx so shutdown can give
	// in-flight work a real chance to finish before cancellation.
	handlerCtx, cancelHandlers := context.WithCancel(context.Background())
	defer cancelHandlers()

	var handlerWG sync.WaitGroup
	sem := make(chan struct{}, updateHandlerConcurrency)
dispatchLoop:
	for upd := range updates {
		select {
		case sem <- struct{}{}:
		case <-handlerCtx.Done():
			break dispatchLoop
		}
		handlerWG.Add(1)
		go func(u telego.Update) {
			defer handlerWG.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					log.Error("update handler panic",
						slog.Any("panic", r),
						slog.String("stack", string(debug.Stack())),
					)
				}
			}()
			handleUpdate(handlerCtx, u, gate, exec, disp, log)
		}(upd)
	}

	// Polling channel closed — graceful shutdown.
	log.Info("polling stopped, waiting for in-flight handlers")
	done := make(chan struct{})
	go func() {
		handlerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Info("all handlers drained")
	case <-time.After(5 * time.Second):
		log.Warn("shutdown grace period exceeded, forcing handler cancellation")
		cancelHandlers()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
	}

	// Let the background loops exit (they observe rootCtx.Done).
	stopDone := make(chan struct{})
	go func() {
		bgWG.Wait()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		log.Warn("background loops did not stop in time")
	}
	return nil
}

// deleteWebhookWithRetry attempts to remove any configured webhook up to
// 3 times with a 2s pause between attempts. Returns the last error on
// persistent failure — startup must abort rather than silently run long
// polling while Telegram still pushes to a webhook.
func deleteWebhookWithRetry(ctx context.Context, tg *telegram.TelegoClient, log *slog.Logger) error {
	const attempts = 3
	const pause = 2 * time.Second
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := tg.DeleteWebhook(ctx); err == nil {
			return nil
		} else {
			lastErr = err
			log.Warn("deleteWebhook attempt failed",
				slog.Int("attempt", i+1),
				slog.Int("max", attempts),
				slog.String("error", err.Error()),
			)
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pause):
			}
		}
	}
	return lastErr
}

func handleUpdate(
	ctx context.Context,
	upd telego.Update,
	gate *gating.Gate,
	exec *gating.Executor,
	disp *commands.Dispatcher,
	log *slog.Logger,
) {
	var (
		msg    *telego.Message
		isEdit bool
	)
	switch {
	case upd.Message != nil:
		msg = upd.Message
	case upd.EditedMessage != nil:
		msg = upd.EditedMessage
		isEdit = true
	case upd.MyChatMember != nil:
		// Log membership changes for the bot itself (useful for observability)
		log.Info("my_chat_member",
			slog.Int64("chat_id", upd.MyChatMember.Chat.ID),
			slog.String("old", upd.MyChatMember.OldChatMember.MemberStatus()),
			slog.String("new", upd.MyChatMember.NewChatMember.MemberStatus()),
		)
		return
	default:
		return
	}
	if msg == nil {
		return
	}

	// Dispatch runs before gating: a group admin who is not a channel member
	// typing `/bind extra` must get a usage reply from the handler rather than
	// have their command message silently deleted by the gate. Handlers are
	// responsible for auto-cleanup of rejection replies (see /bind, /unbind,
	// /status). Pure commands (no trailing text) are still short-circuited out
	// of the gate via IsCommandText; captions are never commands.
	// Messages from a chat identity (sender_chat set) are not user commands; let gate classify them.
	// sender_chat == chat.id → anonymous admin; treat like a user-initiated command for dispatch.
	isUserLikeSender := msg.SenderChat == nil ||
		(msg.SenderChat != nil && msg.SenderChat.ID == msg.Chat.ID)
	if !isEdit && isUserLikeSender {
		handled, err := disp.Dispatch(ctx, msg)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("command handler error", slog.String("error", err.Error()))
		}
		if handled {
			return
		}
	}
	start := time.Now()
	out := gate.Decide(ctx, msg, isEdit)
	var userID int64
	if msg.From != nil {
		userID = msg.From.ID
	}
	exec.Apply(ctx, msg.Chat.ID, msg.MessageID, userID, out, isEdit, time.Since(start))
}

func cleanupLoop(ctx context.Context, st *store.Store, cache *gating.MemberCache, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			n, err := st.DeleteExpiredVerified(ctx, now)
			if err != nil {
				log.Warn("cleanup: deleteExpired", slog.String("error", err.Error()))
				continue
			}
			cache.Prune(now)
			if n > 0 {
				log.Debug("cleanup removed expired verified", slog.Int64("removed", n))
			}
		}
	}
}

// SetupCLIArgs inspects os.Args for `--config <path>` and returns the path (default ./config.yaml).
// Duplicated here to keep the runtime self-contained; the main package can reuse or re-implement.
func SetupCLIArgs() string {
	const def = "./config.yaml"
	for i, a := range os.Args[1:] {
		switch a {
		case "--config", "-c":
			if i+2 < len(os.Args) {
				return os.Args[i+2]
			}
		}
		if v, ok := splitAssign(a, "--config="); ok {
			return v
		}
	}
	return def
}

func splitAssign(a, prefix string) (string, bool) {
	if len(a) > len(prefix) && a[:len(prefix)] == prefix {
		return a[len(prefix):], true
	}
	return "", false
}
