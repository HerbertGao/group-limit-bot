package gating

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/mymmrac/telego"

	"github.com/herbertgao/group-limit-bot/internal/metrics"
	"github.com/herbertgao/group-limit-bot/internal/store"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

// BindingLookup is the subset of the binding service the Gate depends on.
type BindingLookup interface {
	Lookup(ctx context.Context, groupID int64) (*store.Binding, error)
}

// Config holds the injected policy configuration.
type Config struct {
	TTL                 time.Duration
	BotAllowlist        map[int64]bool
	AllowAnonymousAdmin bool
	// IsCommandText returns true if the given message text should be treated as a bot command.
	// Used to short-circuit the command message out of the gating pipeline.
	IsCommandText func(text string) bool
}

// Outcome is the result returned by Decide.
type Outcome struct {
	Decision Decision
	Reason   Reason
	Binding  *store.Binding
	CacheHit bool
}

// Gate runs the decision pipeline.
type Gate struct {
	bindings BindingLookup
	cache    *MemberCache
	dedup    *MediaGroupDedup
	tg       telegram.Client
	metrics  *metrics.Registry
	log      *slog.Logger
	cfg      Config
	now      func() time.Time
}

func New(
	bindings BindingLookup,
	cache *MemberCache,
	dedup *MediaGroupDedup,
	tg telegram.Client,
	reg *metrics.Registry,
	log *slog.Logger,
	cfg Config,
) *Gate {
	return &Gate{
		bindings: bindings,
		cache:    cache,
		dedup:    dedup,
		tg:       tg,
		metrics:  reg,
		log:      log,
		cfg:      cfg,
		now:      time.Now,
	}
}

// Decide runs the pipeline described in design.md. isEdit is carried for logging only.
func (g *Gate) Decide(ctx context.Context, msg *telego.Message, isEdit bool) Outcome {
	chatID := msg.Chat.ID

	binding, err := g.bindings.Lookup(ctx, chatID)
	if err != nil {
		g.log.Warn("binding lookup failed",
			slog.Int64("chat_id", chatID),
			slog.String("error", err.Error()),
		)
		return Outcome{Decision: DecisionIgnore, Reason: ReasonNoBinding}
	}
	if binding == nil {
		return Outcome{Decision: DecisionIgnore, Reason: ReasonNoBinding}
	}

	if isServiceMessage(msg) {
		return Outcome{Decision: DecisionIgnore, Reason: ReasonServiceMessage, Binding: binding}
	}

	// Pure-command short-circuit runs before the sender_chat branch so that
	// `/cmd@other_bot` sent with a channel identity (or a channel root post whose
	// text happens to be a pure command) is preserved rather than deleted.
	if !isEdit && g.cfg.IsCommandText != nil {
		if text := commandBearingText(msg); text != "" && g.cfg.IsCommandText(text) {
			return Outcome{Decision: DecisionIgnore, Reason: ReasonCommand, Binding: binding}
		}
	}

	if msg.SenderChat != nil {
		if msg.SenderChat.ID == binding.ChannelChatID {
			return Outcome{Decision: DecisionAllow, Reason: ReasonChannelRootPost, Binding: binding}
		}
		// Anonymous group admin: Telegram sets sender_chat to the group itself and
		// from.id to GroupAnonymousBot. Must be checked before the generic delete.
		if msg.From != nil &&
			msg.From.ID == telegram.GroupAnonymousBotID &&
			msg.SenderChat.ID == msg.Chat.ID &&
			g.cfg.AllowAnonymousAdmin {
			return Outcome{Decision: DecisionAllow, Reason: ReasonAnonymousAdmin, Binding: binding}
		}
		return Outcome{Decision: DecisionDelete, Reason: ReasonOtherSenderChat, Binding: binding}
	}

	from := msg.From
	if from == nil {
		return Outcome{Decision: DecisionIgnore, Reason: ReasonMissingSender, Binding: binding}
	}

	if from.ID == g.tg.Me().ID {
		return Outcome{Decision: DecisionAllow, Reason: ReasonBotAllowlist, Binding: binding}
	}
	if from.IsBot && g.cfg.BotAllowlist != nil && g.cfg.BotAllowlist[from.ID] {
		return Outcome{Decision: DecisionAllow, Reason: ReasonBotAllowlist, Binding: binding}
	}
	// Defensive: genuine anonymous admin messages always carry sender_chat and are handled above.
	if from.ID == telegram.GroupAnonymousBotID && g.cfg.AllowAnonymousAdmin {
		return Outcome{Decision: DecisionAllow, Reason: ReasonAnonymousAdmin, Binding: binding}
	}

	now := g.now()

	publishOnce := func(dec Decision, rsn Reason) {}
	if msg.MediaGroupID != "" && !isEdit {
		dec, _, hit, leader, aErr := g.dedup.Acquire(ctx, msg.MediaGroupID, now)
		if aErr != nil {
			g.log.Warn("dedup acquire", slog.String("error", aErr.Error()))
		} else if hit {
			return Outcome{Decision: dec, Reason: ReasonMediaGroupDedup, Binding: binding}
		} else if leader {
			id := msg.MediaGroupID
			published := false
			publishOnce = func(dec Decision, rsn Reason) {
				if published {
					return
				}
				published = true
				g.dedup.Publish(id, dec, rsn, now)
			}
			defer func() {
				if !published {
					g.dedup.Abort(id)
				}
			}()
		}
	}

	cached, cacheErr := g.cache.Get(ctx, binding.GroupChatID, binding.ChannelChatID, from.ID, now)
	if cacheErr != nil {
		g.log.Warn("cache get failed",
			slog.Int64("group_id", binding.GroupChatID),
			slog.Int64("user_id", from.ID),
			slog.String("error", cacheErr.Error()),
		)
	}
	if cached {
		out := Outcome{Decision: DecisionAllow, Reason: ReasonCacheHit, Binding: binding, CacheHit: true}
		publishOnce(out.Decision, out.Reason)
		return out
	}

	status, err := g.tg.GetChatMember(ctx, binding.ChannelChatID, from.ID)
	if err != nil {
		attrs := []any{
			slog.Int64("group_id", binding.GroupChatID),
			slog.Int64("channel_id", binding.ChannelChatID),
			slog.Int64("user_id", from.ID),
			slog.String("error", err.Error()),
		}
		var retryAfter time.Duration
		var rle *telegram.RateLimitError
		if errors.As(err, &rle) {
			retryAfter = rle.RetryAfter
			attrs = append(attrs, slog.Duration("retry_after", retryAfter))
		}
		g.log.Warn("getChatMember failed, default allow", attrs...)
		if g.metrics != nil {
			g.metrics.RecordError(metrics.ErrorRecord{
				At:          now,
				Op:          "getChatMember",
				ChatID:      binding.ChannelChatID,
				GroupChatID: binding.GroupChatID,
				UserID:      from.ID,
				Err:         err.Error(),
				RetryAfter:  retryAfter,
			})
		}
		out := Outcome{Decision: DecisionAllow, Reason: ReasonErrorDefaultAllow, Binding: binding}
		publishOnce(out.Decision, out.Reason)
		return out
	}

	if status.InChat() {
		if err := g.cache.Set(ctx, binding.GroupChatID, binding.ChannelChatID, from.ID, binding.Epoch, now); err != nil {
			g.log.Warn("cache set failed",
				slog.Int64("group_id", binding.GroupChatID),
				slog.Int64("user_id", from.ID),
				slog.String("error", err.Error()),
			)
		}
		out := Outcome{Decision: DecisionAllow, Reason: ReasonMember, Binding: binding}
		publishOnce(out.Decision, out.Reason)
		return out
	}

	out := Outcome{Decision: DecisionDelete, Reason: ReasonNotMember, Binding: binding}
	publishOnce(out.Decision, out.Reason)
	return out
}

// commandBearingText returns the user-facing text of a message that a bot
// command could appear in. A real bot command must start with '/' at
// position 0; leading whitespace disqualifies it. Captions on media messages
// are never treated as commands.
func commandBearingText(msg *telego.Message) string {
	s := msg.Text
	if !strings.HasPrefix(s, "/") {
		return ""
	}
	return s
}

// isServiceMessage returns true for Telegram service messages that should be ignored
// entirely by the gating pipeline (joins, leaves, pins, topic changes, etc).
func isServiceMessage(m *telego.Message) bool {
	switch {
	case len(m.NewChatMembers) > 0,
		m.LeftChatMember != nil,
		m.NewChatTitle != "",
		len(m.NewChatPhoto) > 0,
		m.DeleteChatPhoto,
		m.GroupChatCreated,
		m.SupergroupChatCreated,
		m.ChannelChatCreated,
		m.MigrateToChatID != 0,
		m.MigrateFromChatID != 0,
		m.PinnedMessage != nil,
		m.MessageAutoDeleteTimerChanged != nil,
		m.ForumTopicCreated != nil,
		m.ForumTopicEdited != nil,
		m.ForumTopicClosed != nil,
		m.ForumTopicReopened != nil,
		m.GeneralForumTopicHidden != nil,
		m.GeneralForumTopicUnhidden != nil,
		m.VideoChatScheduled != nil,
		m.VideoChatStarted != nil,
		m.VideoChatEnded != nil,
		m.VideoChatParticipantsInvited != nil,
		m.ChatBackgroundSet != nil,
		m.WriteAccessAllowed != nil,
		m.BoostAdded != nil:
		return true
	}
	return false
}
