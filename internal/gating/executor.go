package gating

import (
	"context"
	"log/slog"
	"time"

	"github.com/herbertgao/group-limit-bot/internal/metrics"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

// ViolationStore records guest-summon violations for escalating punishment.
type ViolationStore interface {
	IncrementGuestViolation(ctx context.Context, groupID, userID int64) (int64, error)
}

// GuestPunishPolicy configures escalating punishment of guest-bot summoners.
// MuteThreshold must be >= 2 and BanThreshold > MuteThreshold (enforced by
// config validation), which guarantees the first violation is never punished.
type GuestPunishPolicy struct {
	MuteThreshold int
	BanThreshold  int
	MuteDuration  time.Duration
}

// Executor applies the decision from Decide() to Telegram and records metrics.
type Executor struct {
	tg         telegram.Client
	metrics    *metrics.Registry
	log        *slog.Logger
	violations ViolationStore // nil disables guest-summoner punishment
	policy     GuestPunishPolicy
}

func NewExecutor(tg telegram.Client, reg *metrics.Registry, log *slog.Logger, violations ViolationStore, policy GuestPunishPolicy) *Executor {
	return &Executor{tg: tg, metrics: reg, log: log, violations: violations, policy: policy}
}

// Apply carries out the outcome: deletes the message when DecisionDelete; for a
// guest-bot reply it also deletes the summon message and punishes the summoner.
// It emits a structured decision log and updates metrics.
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
			if !telegram.IsMessageNotFound(err) {
				e.log.Warn("deleteMessage failed", append(attrs, slog.String("error", err.Error()))...)
				return
			}
			// The target message was already gone — the desired end state is
			// reached, so fall through to summon cleanup / punishment.
		} else if e.metrics != nil {
			e.metrics.RecordDelete(chatID, time.Now())
		}
		if out.GuestReply {
			e.handleGuest(ctx, chatID, out, isEdit)
		}
	}

	e.log.Info("gating decision", attrs...)
}

// handleGuest removes the summon message and applies escalating punishment to
// the summoner of an unauthorized guest-bot reply. It runs after the guest
// reply has been deleted (or was found already gone).
func (e *Executor) handleGuest(ctx context.Context, chatID int64, out Outcome, isEdit bool) {
	// Delete the summon message; tolerate it already being gone (the gating
	// pipeline may have deleted a non-follower's summon concurrently).
	if out.GuestSummonMsgID != 0 {
		if err := e.tg.DeleteMessage(ctx, chatID, out.GuestSummonMsgID); err != nil && !telegram.IsMessageNotFound(err) {
			e.log.Warn("guest summon delete failed",
				slog.Int64("group_id", chatID),
				slog.Int("summon_message_id", out.GuestSummonMsgID),
				slog.String("error", err.Error()),
			)
		}
	}

	// Count + punish only on the initial `message` arrival, and only when the
	// caller is a user (a channel/anonymous caller carries no GuestCallerID).
	if isEdit || out.GuestCallerID == 0 || e.violations == nil {
		return
	}
	count, err := e.violations.IncrementGuestViolation(ctx, chatID, out.GuestCallerID)
	if err != nil {
		e.log.Warn("guest violation increment failed",
			slog.Int64("group_id", chatID),
			slog.Int64("caller_id", out.GuestCallerID),
			slog.String("error", err.Error()),
		)
		return
	}
	pAttrs := []any{
		slog.Int64("group_id", chatID),
		slog.Int64("caller_id", out.GuestCallerID),
		slog.Int64("violation_count", count),
	}
	switch {
	case e.policy.BanThreshold > 0 && count >= int64(e.policy.BanThreshold):
		if err := e.tg.BanChatMember(ctx, chatID, out.GuestCallerID); err != nil {
			// restrictChatMember/ban fails on group admins — tolerated by design.
			e.log.Warn("guest summoner ban failed (tolerated)", append(pAttrs, slog.String("error", err.Error()))...)
		} else {
			e.log.Info("guest summoner banned", pAttrs...)
		}
	case e.policy.MuteThreshold > 0 && count >= int64(e.policy.MuteThreshold):
		until := time.Now().Add(e.policy.MuteDuration)
		if err := e.tg.RestrictChatMember(ctx, chatID, out.GuestCallerID, until); err != nil {
			e.log.Warn("guest summoner mute failed (tolerated)", append(pAttrs, slog.String("error", err.Error()))...)
		} else {
			e.log.Info("guest summoner muted", pAttrs...)
		}
	}
}
