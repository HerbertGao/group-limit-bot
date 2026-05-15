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
	BindSvc        *binding.Service
	TG             telegram.Client
	Store          *store.Store
	Metrics        *metrics.Registry
	Cache          *gating.MemberCache
	GroupAllowlist *gating.GroupAllowlist
	Log            *slog.Logger
	// CleanupDelay controls auto-deletion of /bind and /unbind command + reply
	// messages. Zero or negative disables cleanup entirely (useful for tests).
	CleanupDelay time.Duration
}

// Register attaches /bind, /unbind, /status, /allowbot, /disallowbot to the dispatcher.
func (d *Deps) Register(disp *Dispatcher) {
	disp.Register("bind", d.handleBind)
	disp.Register("unbind", d.handleUnbind)
	disp.Register("status", d.handleStatus)
	disp.RegisterArg("allowbot", d.handleAllowBot)
	disp.RegisterArg("disallowbot", d.handleDisallowBot)
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
		// Channel members who aren't the group creator have passed the gate
		// but have no business binding/unbinding — silently delete their
		// command so the bot doesn't become a megaphone replying to them.
		d.silentDelete(ctx, chatID, msg.MessageID)
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
		d.silentDelete(ctx, chatID, msg.MessageID)
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
		d.silentDelete(ctx, chatID, msg.MessageID)
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

	allowedBots, err := d.Store.ListAllowedBots(ctx, chatID)
	if err != nil {
		d.Log.Error("status: list allowed bots", slog.Int64("group_id", chatID), slog.String("error", err.Error()))
		replyID, _ := d.replyText(ctx, chatID, "内部错误,读取 bot 白名单失败")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return err
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
	if len(allowedBots) == 0 {
		fmt.Fprintf(&b2, "群级 bot 白名单: _无_\n")
	} else {
		fmt.Fprintf(&b2, "群级 bot 白名单:\n")
		for _, ab := range allowedBots {
			fmt.Fprintf(&b2, "\\- @%s \\(`%d`\\)\n", telegram.EscapeMarkdownV2(ab.BotUsername), ab.BotUserID)
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

// --------- /allowbot, /disallowbot ---------

func (d *Deps) handleAllowBot(ctx context.Context, msg *telego.Message, args string) error {
	chatID := msg.Chat.ID
	if _, ok := d.requireBoundGroupCreator(ctx, msg); !ok {
		return nil
	}
	botUserID, botUsername, ok := d.resolveBotArg(ctx, chatID, msg, "/allowbot", args)
	if !ok {
		return nil
	}
	created, err := d.Store.AllowBot(ctx, chatID, botUserID, botUsername, msg.From.ID, time.Now())
	if err != nil {
		d.Log.Error("allowbot: store", slog.Int64("group_id", chatID), slog.String("error", err.Error()))
		replyID, _ := d.replyText(ctx, chatID, "内部错误,写入 bot 白名单失败")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return err
	}
	if d.GroupAllowlist != nil {
		d.GroupAllowlist.Invalidate(chatID)
	}
	text := fmt.Sprintf("已将 @%s 加入本群 bot 白名单。", botUsername)
	if !created {
		text = fmt.Sprintf("@%s 已在本群 bot 白名单。", botUsername)
	}
	replyID, _ := d.replyText(ctx, chatID, text)
	d.scheduleCleanup(chatID, msg.MessageID, replyID)
	return nil
}

func (d *Deps) handleDisallowBot(ctx context.Context, msg *telego.Message, args string) error {
	chatID := msg.Chat.ID
	if _, ok := d.requireBoundGroupCreator(ctx, msg); !ok {
		return nil
	}
	botUserID, botUsername, ok := d.resolveBotArg(ctx, chatID, msg, "/disallowbot", args)
	if !ok {
		return nil
	}
	removed, err := d.Store.DisallowBot(ctx, chatID, botUserID)
	if err != nil {
		d.Log.Error("disallowbot: store", slog.Int64("group_id", chatID), slog.String("error", err.Error()))
		replyID, _ := d.replyText(ctx, chatID, "内部错误,移除 bot 白名单失败")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return err
	}
	if d.GroupAllowlist != nil {
		d.GroupAllowlist.Invalidate(chatID)
	}
	text := fmt.Sprintf("已将 @%s 从本群 bot 白名单移除。", botUsername)
	if !removed {
		text = fmt.Sprintf("@%s 不在本群 bot 白名单。", botUsername)
	}
	replyID, _ := d.replyText(ctx, chatID, text)
	d.scheduleCleanup(chatID, msg.MessageID, replyID)
	return nil
}

// requireBoundGroupCreator validates that a command runs inside a bound
// discussion group and is invoked by that group's creator. On failure it has
// already sent the appropriate reply / silent-delete and returns ok=false.
func (d *Deps) requireBoundGroupCreator(ctx context.Context, msg *telego.Message) (*store.Binding, bool) {
	chatID := msg.Chat.ID
	if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		replyID, _ := d.replyText(ctx, chatID, "请在评论群内执行本命令")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil, false
	}
	if msg.From == nil {
		d.silentDelete(ctx, chatID, msg.MessageID)
		return nil, false
	}
	callerStatus, err := d.TG.GetChatMember(ctx, chatID, msg.From.ID)
	if err != nil {
		d.Log.Warn("command: getChatMember caller failed", slog.String("error", err.Error()))
		replyID, _ := d.replyText(ctx, chatID, "无法校验创建者身份,请稍后重试")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil, false
	}
	if !callerStatus.IsCreator() {
		d.silentDelete(ctx, chatID, msg.MessageID)
		return nil, false
	}
	b, err := d.Store.GetBinding(ctx, chatID)
	if err != nil {
		d.Log.Error("command: load binding", slog.Int64("group_id", chatID), slog.String("error", err.Error()))
		replyID, _ := d.replyText(ctx, chatID, "内部错误,读取绑定失败")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil, false
	}
	if b == nil {
		replyID, _ := d.replyText(ctx, chatID, "当前群未绑定任何频道")
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return nil, false
	}
	return b, true
}

// resolveBotArg resolves the bot targeted by /allowbot or /disallowbot, from a
// reply-to a bot's message or from the @username argument. On any failure it
// sends an error reply and returns ok=false.
func (d *Deps) resolveBotArg(ctx context.Context, chatID int64, msg *telego.Message, cmd, args string) (botID int64, username string, ok bool) {
	if r := msg.ReplyToMessage; r != nil && r.From != nil && r.From.IsBot {
		return r.From.ID, r.From.Username, true
	}
	name := strings.TrimSpace(args)
	if i := strings.IndexAny(name, " \t\n\r"); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimPrefix(name, "@")
	if name == "" {
		replyID, _ := d.replyText(ctx, chatID, fmt.Sprintf("用法: %s @bot用户名 (或回复某 bot 的消息执行)", cmd))
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return 0, "", false
	}
	info, err := d.TG.ResolveUsername(ctx, name)
	if err != nil {
		d.Log.Warn("resolve bot username failed", slog.String("username", name), slog.String("error", err.Error()))
		replyID, _ := d.replyText(ctx, chatID, fmt.Sprintf("无法解析用户名 @%s,请确认拼写正确。", name))
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return 0, "", false
	}
	if info.ID <= 0 {
		replyID, _ := d.replyText(ctx, chatID, fmt.Sprintf("@%s 不是用户/bot(可能是频道或群),无法加入 bot 白名单。", name))
		d.scheduleCleanup(chatID, msg.MessageID, replyID)
		return 0, "", false
	}
	if info.Username != "" {
		name = info.Username
	}
	return info.ID, name, true
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

// silentDelete removes a command message synchronously without sending any
// reply. Used for non-creator rejections on /bind, /unbind, /status so the
// bot doesn't broadcast an interaction with the unauthorized sender.
// Failures are logged at warning level — if the bot has lost delete
// permission, unauthorized commands stay visible and operators need a
// signal louder than debug to notice.
func (d *Deps) silentDelete(ctx context.Context, chatID int64, messageID int) {
	if err := d.TG.DeleteMessage(ctx, chatID, messageID); err != nil {
		d.Log.Warn("silent delete failed",
			slog.Int64("chat_id", chatID),
			slog.Int("message_id", messageID),
			slog.String("error", err.Error()),
		)
	}
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
