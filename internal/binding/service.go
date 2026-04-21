package binding

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/herbertgao/group-limit-bot/internal/store"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

// User-facing errors. Use errors.Is to compare.
var (
	ErrNotGroup              = errors.New("bind command must be run inside a group or supergroup")
	ErrCallerNotAdmin        = errors.New("caller is not the group creator")
	ErrNoLinkedChannel       = errors.New("this group has no linked discussion channel — link it in the channel settings first")
	ErrBotNotChannelAdmin    = errors.New("bot is not an administrator in the linked channel")
	ErrBotCannotModerateGroup = errors.New("bot is missing admin delete permission in the discussion group")
	ErrNotBound              = errors.New("this group is not bound to any channel")
)

type Service struct {
	store *store.Store
	tg    telegram.Client
	now   func() time.Time
}

func New(st *store.Store, tg telegram.Client) *Service {
	return &Service{store: st, tg: tg, now: time.Now}
}

// Result is returned by Bind so callers can distinguish "created" vs "updated".
type Result struct {
	Binding        store.Binding
	WasCreated     bool
	ChannelChanged bool
	ChannelTitle   string
	ChannelUser    string
	GroupTitle     string
}

// Bind performs the full precondition-checked binding flow.
// groupChatInfo must be the chat where the /bind command was received;
// it is passed in (instead of re-fetched) so the caller can read it from the message.
// Caller must be the group creator; anonymous admins (user id 1087968824) are
// naturally rejected because getChatMember returns a non-creator status for them.
func (s *Service) Bind(ctx context.Context, groupChat *telegram.ChatInfo, callerUserID int64) (*Result, error) {
	if groupChat.Type != "group" && groupChat.Type != "supergroup" {
		return nil, ErrNotGroup
	}

	callerStatus, err := s.tg.GetChatMember(ctx, groupChat.ID, callerUserID)
	if err != nil {
		return nil, fmt.Errorf("check caller admin: %w", err)
	}
	if !callerStatus.IsCreator() {
		return nil, ErrCallerNotAdmin
	}

	// Re-fetch full chat to read linked_chat_id authoritatively.
	full, err := s.tg.GetChat(ctx, groupChat.ID)
	if err != nil {
		return nil, fmt.Errorf("get group chat: %w", err)
	}
	if full.LinkedChatID == 0 {
		return nil, ErrNoLinkedChannel
	}

	// Check bot is admin in linked channel.
	botStatus, err := s.tg.GetChatMember(ctx, full.LinkedChatID, s.tg.Me().ID)
	if err != nil {
		if isTransientAPIErr(err) {
			return nil, fmt.Errorf("check bot admin in channel: %w", err)
		}
		return nil, ErrBotNotChannelAdmin
	}
	if !botStatus.IsAdmin() {
		return nil, ErrBotNotChannelAdmin
	}

	// Check bot can delete messages in the discussion group.
	canDelete, err := s.tg.GetChatMemberCanDelete(ctx, groupChat.ID, s.tg.Me().ID)
	if err != nil {
		if isTransientAPIErr(err) {
			return nil, fmt.Errorf("check bot moderate group: %w", err)
		}
		return nil, ErrBotCannotModerateGroup
	}
	if !canDelete {
		return nil, ErrBotCannotModerateGroup
	}

	// Pull channel title for reply formatting.
	channel, err := s.tg.GetChat(ctx, full.LinkedChatID)
	if err != nil {
		return nil, fmt.Errorf("get channel chat: %w", err)
	}

	b := store.Binding{
		GroupChatID:   groupChat.ID,
		ChannelChatID: full.LinkedChatID,
		BoundByUserID: callerUserID,
		BoundAt:       s.now(),
	}
	created, channelChanged, err := s.store.UpsertBinding(ctx, b)
	if err != nil {
		return nil, fmt.Errorf("upsert binding: %w", err)
	}
	return &Result{
		Binding:        b,
		WasCreated:     created,
		ChannelChanged: channelChanged,
		ChannelTitle:   channel.Title,
		ChannelUser:    channel.Username,
		GroupTitle:     full.Title,
	}, nil
}

// Unbind removes the binding if caller is the group creator. Returns ErrNotBound when no binding exists.
// Anonymous admins are naturally rejected because getChatMember returns non-creator for user id 1087968824.
func (s *Service) Unbind(ctx context.Context, groupID int64, callerUserID int64) error {
	callerStatus, err := s.tg.GetChatMember(ctx, groupID, callerUserID)
	if err != nil {
		return fmt.Errorf("check caller admin: %w", err)
	}
	if !callerStatus.IsCreator() {
		return ErrCallerNotAdmin
	}

	removed, err := s.store.DeleteBinding(ctx, groupID)
	if err != nil {
		return fmt.Errorf("delete binding: %w", err)
	}
	if !removed {
		return ErrNotBound
	}
	return nil
}

// Lookup returns the current binding for groupID, or nil if none.
// Fast path for gating; no permission checks.
func (s *Service) Lookup(ctx context.Context, groupID int64) (*store.Binding, error) {
	return s.store.GetBinding(ctx, groupID)
}

// isTransientAPIErr reports whether err is a transient Telegram error we should
// bubble up (429 rate limit, timeout, cancellation). All other errors during a
// bot self-check indicate a permissions/access problem and should be surfaced as
// the corresponding Err* sentinel.
func isTransientAPIErr(err error) bool {
	if err == nil {
		return false
	}
	var rle *telegram.RateLimitError
	if errors.As(err, &rle) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	return false
}
