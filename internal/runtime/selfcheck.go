package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/herbertgao/group-limit-bot/internal/store"
	"github.com/herbertgao/group-limit-bot/internal/telegram"
)

// isTransient reports whether an error from Telegram API calls should be
// treated as a temporary/retryable condition (rate limits, deadlines,
// cancellation) rather than a degradation signal worth alerting on.
func isTransient(err error) bool {
	if err == nil {
		return true
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

// SelfCheck periodically verifies the bot is still an admin in every bound channel.
// When a binding has become degraded, it posts a single warning message to the
// corresponding group, throttled to at most once per hour per group.
type SelfCheck struct {
	tg  telegram.Client
	st  *store.Store
	log *slog.Logger

	mu          sync.Mutex
	lastWarned  map[int64]time.Time
	warnCooldown time.Duration
}

func NewSelfCheck(tg telegram.Client, st *store.Store, log *slog.Logger) *SelfCheck {
	return &SelfCheck{
		tg:           tg,
		st:           st,
		log:          log,
		lastWarned:   make(map[int64]time.Time),
		warnCooldown: time.Hour,
	}
}

// Run loops until ctx is done, ticking every interval.
func (sc *SelfCheck) Run(ctx context.Context, interval time.Duration) {
	// Run once immediately, then on a ticker.
	sc.tick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.tick(ctx)
		}
	}
}

func (sc *SelfCheck) tick(ctx context.Context) {
	bindings, err := sc.st.ListBindings(ctx)
	if err != nil {
		sc.log.Warn("selfcheck: list bindings", slog.String("error", err.Error()))
		return
	}
	botID := sc.tg.Me().ID
	for _, b := range bindings {
		degradedReasons, channelOK, groupOK := sc.checkBinding(ctx, b, botID)
		if len(degradedReasons) > 0 {
			// Run ticks serially, so canWarn/markWarned need not be atomic together.
			if !sc.canWarn(b.GroupChatID) {
				continue
			}
			text := "⚠️ bot 监护已降级(守卫将默认放行):" + strings.Join(degradedReasons, ";") + "。请检查 bot 权限。"
			if _, err := sc.tg.SendMessage(ctx, b.GroupChatID, text, false); err != nil {
				sc.log.Warn("selfcheck: send warning",
					slog.Int64("group_id", b.GroupChatID),
					slog.String("error", err.Error()),
				)
				continue
			}
			sc.markWarned(b.GroupChatID)
			continue
		}
		// Only clear cooldown when BOTH checks returned a positive clean result.
		// Transient errors leave cooldown untouched to prevent intermittent
		// flakiness from resetting the 1h throttle.
		if channelOK && groupOK {
			sc.clearWarned(b.GroupChatID)
		}
	}
}

// checkBinding runs the two health checks for a single binding and classifies
// the outcome. Returns the degraded reasons (empty when not degraded), and
// per-dimension positive-clean flags. A transient error leaves the
// corresponding *OK flag false without adding a reason.
func (sc *SelfCheck) checkBinding(ctx context.Context, b store.Binding, botID int64) (reasons []string, channelOK, groupOK bool) {
	status, err := sc.tg.GetChatMember(ctx, b.ChannelChatID, botID)
	switch {
	case err != nil && isTransient(err):
		sc.log.Warn("selfcheck: transient error on channel",
			slog.Int64("channel_id", b.ChannelChatID),
			slog.String("error", err.Error()),
		)
	case err != nil:
		sc.log.Warn("selfcheck: get bot status",
			slog.Int64("channel_id", b.ChannelChatID),
			slog.String("error", err.Error()),
		)
		reasons = append(reasons, fmt.Sprintf("频道查询失败:%s", err.Error()))
	case !status.IsAdmin():
		reasons = append(reasons, fmt.Sprintf("频道当前状态 %s,非 administrator", status))
	default:
		channelOK = true
	}
	canDelete, gErr := sc.tg.GetChatMemberCanDelete(ctx, b.GroupChatID, botID)
	switch {
	case gErr != nil && isTransient(gErr):
		sc.log.Warn("selfcheck: transient error on group",
			slog.Int64("group_id", b.GroupChatID),
			slog.String("error", gErr.Error()),
		)
	case gErr != nil:
		sc.log.Warn("selfcheck: get bot group delete perm",
			slog.Int64("group_id", b.GroupChatID),
			slog.String("error", gErr.Error()),
		)
		reasons = append(reasons, fmt.Sprintf("群权限查询失败:%s", gErr.Error()))
	case !canDelete:
		reasons = append(reasons, "bot 在本群已失去删除消息权限")
	default:
		groupOK = true
	}
	return reasons, channelOK, groupOK
}

func (sc *SelfCheck) canWarn(groupID int64) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	last, ok := sc.lastWarned[groupID]
	if !ok {
		return true
	}
	return time.Since(last) >= sc.warnCooldown
}

func (sc *SelfCheck) markWarned(groupID int64) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.lastWarned[groupID] = time.Now()
}

// clearWarned forgets the last-warned timestamp for groupID. Called when
// the group is healthy again so that a later degradation fires an alert
// immediately instead of being suppressed by a stale cooldown.
func (sc *SelfCheck) clearWarned(groupID int64) {
	sc.mu.Lock()
	delete(sc.lastWarned, groupID)
	sc.mu.Unlock()
}
