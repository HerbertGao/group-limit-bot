package gating

import (
	"context"
	"log/slog"
	"time"

	"github.com/herbertgao/group-limit-bot/internal/metrics"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

// Executor applies the decision from Decide() to Telegram and records metrics.
type Executor struct {
	tg      telegram.Client
	metrics *metrics.Registry
	log     *slog.Logger
}

func NewExecutor(tg telegram.Client, reg *metrics.Registry, log *slog.Logger) *Executor {
	return &Executor{tg: tg, metrics: reg, log: log}
}

// Apply carries out the outcome: deletes the message when DecisionDelete; otherwise no-op.
// It also emits a structured decision log and updates metrics.
func (e *Executor) Apply(ctx context.Context, chatID int64, messageID int, userID int64, out Outcome, isEdit bool, latency time.Duration) {
	attrs := []any{
		slog.Int64("group_id", chatID),
		slog.Int64("user_id", userID),
		slog.String("decision", out.Decision.String()),
		slog.String("reason", string(out.Reason)),
		slog.Bool("cache_hit", out.CacheHit),
		slog.Int64("latency_ms", latency.Milliseconds()),
		slog.Bool("is_edit", isEdit),
		slog.Int("message_id", messageID),
	}

	if out.Decision == DecisionDelete {
		if err := e.tg.DeleteMessage(ctx, chatID, messageID); err != nil {
			e.log.Warn("deleteMessage failed", append(attrs, slog.String("error", err.Error()))...)
			return
		}
		if e.metrics != nil {
			e.metrics.RecordDelete(chatID, time.Now())
		}
	}

	e.log.Info("gating decision", attrs...)
}
