package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mymmrac/telego"

	"github.com/herbertgao/group-limit-bot/internal/binding"
	"github.com/herbertgao/group-limit-bot/internal/gating"
	"github.com/herbertgao/group-limit-bot/internal/metrics"
	"github.com/herbertgao/group-limit-bot/internal/store"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

// Deps wires everything the admin command handlers need.
type Deps struct {
	BindSvc *binding.Service
	TG      telegram.Client
	Store   *store.Store
	Metrics *metrics.Registry
	Cache   *gating.MemberCache
	Log     *slog.Logger
	// CleanupDelay controls auto-deletion of /bind and /unbind command + reply
	// messages. Zero or negative disables cleanup entirely (useful for tests).
	CleanupDelay time.Duration
}

// Register attaches /bind, /unbind, and /status to the dispatcher.
func (d *Deps) Register(disp *Dispatcher) {
	disp.Register("bind", d.handleBind)
	disp.Register("unbind", d.handleUnbind)
	disp.Register("status", d.handleStatus)
}

// --------- /bind ---------

func (d *Deps) handleBind(ctx context.Context, msg *telego.Message, args string) error {
	chatID := msg.Chat.ID
	if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		replyID, _ := d.replyText(ctx, chatID, "请在评论群内执行本命令")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil
	}
	if msg.From == nil {
		return errors.New("missing sender")
	}
	groupInfo := &telegram.ChatInfo{
		ID:    msg.Chat.ID,
		Type:  msg.Chat.Type,
		Title: msg.Chat.Title,
	}
	res, err := d.BindSvc.Bind(ctx, groupInfo, msg.From.ID)
	switch {
	case errors.Is(err, binding.ErrNotGroup):
		replyID, _ := d.replyText(ctx, chatID, "请在评论群内执行本命令")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil
	case errors.Is(err, binding.ErrCallerNotAdmin):
		replyID, _ := d.replyText(ctx, chatID, "仅群创建者可执行本命令")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil
	case errors.Is(err, binding.ErrNoLinkedChannel):
		replyID, _ := d.replyText(ctx, chatID, "当前群未关联讨论频道。请先在对应频道设置中将本群设为 discussion group。")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil
	case errors.Is(err, binding.ErrBotNotChannelAdmin):
		replyID, _ := d.replyText(ctx, chatID, "bot 尚未被加为绑定频道的管理员,请先在频道管理员列表中添加 bot 再执行 /bind。")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil
	case errors.Is(err, binding.ErrBotCannotModerateGroup):
		replyID, _ := d.replyText(ctx, chatID, "bot 在本群没有删除消息权限,请在群管理员设置中授予 bot '删除消息' 权限后再执行 /bind。")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil
	case err != nil:
		d.Log.Error("bind failed", slog.Int64("group_id", chatID), slog.String("error", err.Error()))
		replyID, _ := d.replyText(ctx, chatID, "内部错误,绑定失败,请稍后再试")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return err
	}

	verb := "已创建"
	if !res.WasCreated {
		verb = "已更新"
	}
	// Drop the in-memory cache for this group when the binding now points at a different
	// channel; verified rows were already wiped in the same store tx.
	if res.ChannelChanged && d.Cache != nil {
		d.Cache.DropGroup(chatID)
	}
	title := telegram.EscapeMarkdownV2(res.ChannelTitle)
	grp := telegram.EscapeMarkdownV2(res.GroupTitle)
	channelRef := title
	if res.ChannelUser != "" {
		channelRef = fmt.Sprintf("[%s](https://t.me/%s)", title, res.ChannelUser)
	}
	text := fmt.Sprintf(
		"*%s绑定*\n群组: %s\n频道: %s\n\n未关注该频道的用户消息将被静默删除。",
		telegram.EscapeMarkdownV2(verb), grp, channelRef,
	)
	replyID, sendErr := d.reply(ctx, chatID, text, true)
	d.scheduleCleanup(chatID, msg.MessageID, replyID)
	if sendErr != nil {
		d.Log.Error("bind succeeded but reply failed",
			slog.Int64("group_id", chatID),
			slog.String("error", sendErr.Error()),
		)
		return fmt.Errorf("bind reply: %w", sendErr)
	}
	return nil
}

// --------- /unbind ---------

func (d *Deps) handleUnbind(ctx context.Context, msg *telego.Message, _ string) error {
	chatID := msg.Chat.ID
	if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		replyID, _ := d.replyText(ctx, chatID, "请在评论群内执行本命令")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil
	}
	if msg.From == nil {
		return errors.New("missing sender")
	}
	err := d.BindSvc.Unbind(ctx, chatID, msg.From.ID)
	switch {
	case errors.Is(err, binding.ErrCallerNotAdmin):
		replyID, _ := d.replyText(ctx, chatID, "仅群创建者可执行本命令")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
	case errors.Is(err, binding.ErrNotBound):
		replyID, _ := d.replyText(ctx, chatID, "当前群未绑定任何频道")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
	case err != nil:
		d.Log.Error("unbind failed", slog.Int64("group_id", chatID), slog.String("error", err.Error()))
		replyID, _ := d.replyText(ctx, chatID, "内部错误,解绑失败,请稍后再试")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return err
	default:
		if d.Cache != nil {
			d.Cache.DropGroup(chatID)
		}
		replyID, _ := d.replyText(ctx, chatID, "已解除绑定,缓存已清空。")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
	}
	return nil
}

// --------- /status ---------

func (d *Deps) handleStatus(ctx context.Context, msg *telego.Message, _ string) error {
	chatID := msg.Chat.ID
	if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		replyID, _ := d.replyText(ctx, chatID, "请在评论群内执行本命令")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil
	}
	if msg.From == nil {
		return errors.New("missing sender")
	}
	callerStatus, err := d.TG.GetChatMember(ctx, chatID, msg.From.ID)
	if err != nil {
		d.Log.Warn("status: getChatMember caller failed", slog.String("error", err.Error()))
		replyID, _ := d.replyText(ctx, chatID, "无法校验创建者身份,请稍后重试")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil
	}
	if !callerStatus.IsCreator() {
		replyID, _ := d.replyText(ctx, chatID, "仅群创建者可执行本命令")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil
	}

	b, err := d.Store.GetBinding(ctx, chatID)
	if err != nil {
		d.Log.Error("status: load binding", slog.String("error", err.Error()))
		replyID, _ := d.replyText(ctx, chatID, "内部错误,读取绑定失败")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return err
	}
	if b == nil {
		replyID, _ := d.replyText(ctx, chatID, "当前群未绑定任何频道")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil
	}

	now := time.Now()
	verifiedCount, err := d.Store.CountVerifiedInChannel(ctx, chatID, b.ChannelChatID, now)
	if err != nil {
		d.Log.Error("status: count verified", slog.String("error", err.Error()))
		replyID, _ := d.replyText(ctx, chatID, "内部错误,读取状态失败")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return err
	}
	deletes := 0
	if d.Metrics != nil {
		deletes = d.Metrics.CountRecentDeletes(chatID, now)
	}

	info, err := d.TG.GetChat(ctx, b.ChannelChatID)
	if err != nil {
		d.Log.Error("status: get channel metadata",
			slog.Int64("channel_id", b.ChannelChatID),
			slog.String("error", err.Error()),
		)
		replyID, _ := d.replyText(ctx, chatID, "内部错误,读取频道信息失败(bot 可能已失去频道访问权)")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return err
	}
	channelTitle := info.Title
	channelRef := info.Username

	var errLines []string
	if d.Metrics != nil {
		for _, e := range d.Metrics.RecentErrorsForGroup(chatID) {
			errLines = append(errLines,
				fmt.Sprintf("`%s` %s: %s",
					telegram.EscapeMarkdownV2(e.At.Format("2006-01-02 15:04:05")),
					telegram.EscapeMarkdownV2(e.Op),
					telegram.EscapeMarkdownV2(truncate(e.Err, 80)),
				))
		}
	}

	chRefEsc := telegram.EscapeMarkdownV2(channelTitle)
	if channelRef != "" {
		chRefEsc = fmt.Sprintf("[%s](https://t.me/%s)", telegram.EscapeMarkdownV2(channelTitle), channelRef)
	}

	var b2 strings.Builder
	fmt.Fprintf(&b2, "*状态*\n")
	fmt.Fprintf(&b2, "群组: `%d`\n", chatID)
	fmt.Fprintf(&b2, "频道: %s \\(`%d`\\)\n", chRefEsc, b.ChannelChatID)
	fmt.Fprintf(&b2, "已验证成员: %d\n", verifiedCount)
	fmt.Fprintf(&b2, "近 1 小时删除: %d\n", deletes)
	if len(errLines) == 0 {
		fmt.Fprintf(&b2, "最近错误: _无_\n")
	} else {
		fmt.Fprintf(&b2, "最近错误:\n")
		for _, l := range errLines {
			fmt.Fprintf(&b2, "\\- %s\n", l)
		}
	}
	if _, err := d.reply(ctx, chatID, b2.String(), true); err != nil {
		d.Log.Error("status: send report failed",
			slog.Int64("group_id", chatID),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("status reply: %w", err)
	}
	return nil
}

// --------- helpers ---------

// reply sends text as a reply; returns the sent message ID (0 on failure).
func (d *Deps) reply(ctx context.Context, chatID int64, text string, markdownV2 bool) (int, error) {
	id, err := d.TG.SendMessage(ctx, chatID, text, markdownV2)
	if err != nil {
		d.Log.Warn("reply failed",
			slog.Int64("chat_id", chatID),
			slog.String("error", err.Error()),
		)
		return 0, err
	}
	return id, nil
}

// replyText sends a plain Chinese text reply, escaped for MarkdownV2 consistency.
func (d *Deps) replyText(ctx context.Context, chatID int64, text string) (int, error) {
	return d.reply(ctx, chatID, telegram.EscapeMarkdownV2(text), true)
}

// scheduleCleanup asynchronously deletes (chatID, messageIDs...) after
// d.CleanupDelay. Zero-valued message IDs are skipped (SendMessage failure).
// When d.CleanupDelay <= 0, cleanup is disabled — useful for tests. Cleanup is
// best-effort; failures are logged at debug level only.
func (d *Deps) scheduleCleanup(chatID int64, messageIDs ...int) {
	if d.CleanupDelay <= 0 {
		return
	}
	// Filter zero IDs.
	ids := make([]int, 0, len(messageIDs))
	for _, id := range messageIDs {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	delay := d.CleanupDelay
	go func() {
		time.Sleep(delay)
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := d.TG.DeleteMessage(ctx, chatID, id); err != nil {
				d.Log.Debug("cleanup delete failed",
					slog.Int64("chat_id", chatID),
					slog.Int("message_id", id),
					slog.String("error", err.Error()),
				)
			}
			cancel()
		}
	}()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
