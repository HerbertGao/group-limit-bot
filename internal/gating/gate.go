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
	// GroupAllowlist resolves the per-group bot allowlist. Nil is valid and
	// means only the global BotAllowlist applies.
	GroupAllowlist *GroupAllowlist
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

	// Guest-bot fields, set only when Reason == ReasonGuestBot.
	GuestReply       bool  // true when this outcome classified a guest-bot reply
	GuestSummonMsgID int   // reply_to_message.message_id of the summon; 0 if absent
	GuestCallerID    int64 // guest_bot_caller_user.id; 0 when the caller is not a user
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
//
// Pure command short-circuit is applied AFTER the access pipeline: only senders
// that would otherwise be allowed (member, channel root, anonymous admin, etc.)
// have a `/cmd` / `/cmd@other_bot` message demoted from Allow → Ignore/Command.
// Non-members and unbound channel identities still go through the normal
// delete path even when their text looks command-shaped — otherwise any
// stranger could post `/spam@anywhere` and the gate would protect them.
func (g *Gate) Decide(ctx context.Context, msg *telego.Message, isEdit bool) Outcome {
	out := g.decideBase(ctx, msg, isEdit)
	return g.demoteAllowedCommand(out, msg, isEdit)
}

// demoteAllowedCommand converts an Allow outcome to Ignore/Command when the
// message is a pure command and the Allow came from an explicit access check.
// Delete outcomes are returned unchanged so that non-members can't escape
// moderation by command-shaping their text. ReasonErrorDefaultAllow is
// excluded by design — Telegram API failures must not silently re-open the
// command dispatcher to non-members.
func (g *Gate) demoteAllowedCommand(out Outcome, msg *telego.Message, isEdit bool) Outcome {
	if out.Decision != DecisionAllow {
		return out
	}
	if !isVerifiedAccessReason(out.Reason) {
		return out
	}
	if isEdit || g.cfg.IsCommandText == nil {
		return out
	}
	text := commandBearingText(msg)
	if text == "" || !g.cfg.IsCommandText(text) {
		return out
	}
	out.Decision = DecisionIgnore
	out.Reason = ReasonCommand
	return out
}

// isVerifiedAccessReason reports whether an Allow outcome was produced by a
// positive access check rather than a fail-open default. Only verified
// reasons are eligible for command-demotion so a Telegram API outage cannot
// reopen the dispatcher to non-members via ReasonErrorDefaultAllow.
func isVerifiedAccessReason(r Reason) bool {
	switch r {
	case ReasonMember, ReasonCacheHit, ReasonChannelRootPost,
		ReasonAnonymousAdmin, ReasonBotAllowlist:
		return true
	}
	return false
}

// decideBase is the original access-classification pipeline. It produces
// Allow / Delete / Ignore outcomes purely on sender identity and membership;
// the caller (Decide) decides whether to demote an Allow to Ignore/Command.
func (g *Gate) decideBase(ctx context.Context, msg *telego.Message, isEdit bool) Outcome {
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

	// Guest-bot reply detection runs at the front of the pipeline — before the
	// sender_chat branch, the bot-allowlist short-circuit and getChatMember —
	// so a guest reply that also carries sender_chat cannot be mis-classified
	// as a channel root post or escape via another branch.
	if isGuestReply(msg) {
		return g.decideGuest(ctx, msg, binding)
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
	if from.IsBot && g.botAllowed(ctx, binding.GroupChatID, from.ID) {
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

// botAllowed reports whether botID is allowed in groupID via the global
// allowlist or the group's per-group allowlist.
func (g *Gate) botAllowed(ctx context.Context, groupID, botID int64) bool {
	if g.cfg.BotAllowlist != nil && g.cfg.BotAllowlist[botID] {
		return true
	}
	return g.cfg.GroupAllowlist.Allowed(ctx, groupID, botID)
}

// isGuestReply reports whether msg is a reply produced by a Telegram guest bot.
// The deterministic markers are the guest_bot_caller_user / guest_bot_caller_chat
// fields and guest_query_id; any one being set identifies a guest reply.
func isGuestReply(m *telego.Message) bool {
	return m.GuestBotCallerUser != nil || m.GuestBotCallerChat != nil || m.GuestQueryID != ""
}

// decideGuest classifies a guest-bot reply. A reply from a bot in the effective
// allowlist (global ∪ per-group, plus the guardian bot itself) is allowed;
// otherwise it is deleted, carrying the summon message id and the caller id so
// the executor can also remove the summon and punish the summoner.
func (g *Gate) decideGuest(ctx context.Context, msg *telego.Message, binding *store.Binding) Outcome {
	var botID int64
	if msg.From != nil {
		botID = msg.From.ID
	}
	if botID != 0 && (botID == g.tg.Me().ID || g.botAllowed(ctx, binding.GroupChatID, botID)) {
		return Outcome{Decision: DecisionAllow, Reason: ReasonBotAllowlist, Binding: binding}
	}
	out := Outcome{
		Decision:   DecisionDelete,
		Reason:     ReasonGuestBot,
		Binding:    binding,
		GuestReply: true,
	}
	if msg.ReplyToMessage != nil {
		out.GuestSummonMsgID = msg.ReplyToMessage.MessageID
	}
	if msg.GuestBotCallerUser != nil {
		out.GuestCallerID = msg.GuestBotCallerUser.ID
	}
	return out
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
