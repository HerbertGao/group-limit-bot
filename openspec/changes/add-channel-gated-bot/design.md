## 上下文

项目从零起步,目录除 `openspec/` 脚手架外无任何代码。目标是一个独立运行的守卫 bot,保护**已存在**的 Telegram 频道及其 linked 评论群。核心业务规则由之前的探索阶段敲定:

- 懒校验 + 静默删除,不主动打扰用户;政策通过频道/群公告向用户说明。
- 单 bot 实例服务多对"频道 ↔ 评论群",绑定关系持久化。
- 运营者必须是群**创建者**(通常也是频道主,linked 结构的典型配置),`/bind` 权限通过精确匹配 `MemberStatus == creator` 保证,匿名管理员自然被拒绝。

约束:

- Telegram Bot API 全局限速约 30 req/s,需要缓存和节流才不会撞 429。
- bot 必须是目标频道的管理员才能 `getChatMember` 查询任意用户成员状态,这是 Telegram 的硬要求。
- bot 必须关闭 Privacy Mode(通过 `@BotFather` `/setprivacy` 设为 Disable),否则群里只能收到命令消息,收不到普通评论。
- 单机单进程部署;没有高可用需求,重启/崩溃期间评论群暂时不受守护是可接受的。

利益相关者:频道主/群管理员(部署和配置),评论群发言用户(被透明守护或静默删除)。

## 目标 / 非目标

**目标:**

- 在绑定评论群中,**未关注频道的用户消息在可观察时间内被静默删除**(目标: 从消息落地到删除 P50 < 1s、P95 < 3s)。
- 单 bot 可承载 10~50 对(频道, 群),无需额外基础设施。
- 配置绑定只需一条群内命令,不需要手填 chat id。
- 重启后绑定关系和"已通过成员"缓存持久化恢复。
- 观测性足够诊断"为什么这条应该删的没删/不该删的被删了":日志带 chat_id、user_id、decision、reason。

**非目标:**

- 不做反垃圾内容识别、图片审核、CAPTCHA、验证码。策略**只**是"是否关注绑定频道"。
- 不做主动限制成员(`restrictChatMember`)流程——评论群场景下用户不是"加群",删消息即可。
- 不做 webhook 模式、不做多实例横向扩展、不做 HA。
- 不做网页管理后台、不做多语言、不做计费/多租户隔离。
- 不做"关注过就放行(忽略事后取关)"或"强制每条都查"等可配置策略的多模式——一律短 TTL 缓存策略。
- 不做主动扫描群历史成员(Telegram Bot 无此能力)。

## 决策

### 语言与核心库

**选型: Go + `github.com/mymmrac/telego`。**

- Go 的单二进制部署和并发模型适合长轮询事件驱动场景。
- `telego` 相比 `go-telegram-bot-api/v5` 跟进 Bot API 7.x、类型完整、API 生成自官方 schema,维护活跃。相比 `tucnak/telebot/v3`,`telego` 更贴近 raw API,中间件和 update 分发器足够灵活,控制力更强。
- **考虑过的替代方案**:
  - `go-telegram-bot-api/v5`: API 陈旧、维护缓慢,放弃。
  - `telebot/v3`: DSL 顺手,但隐藏了太多 update 细节,排查 edge case(sender_chat、media group)时不方便。
  - `go-telegram/bot`: 极简零依赖,但生态和文档较 `telego` 弱。

### 持久化

**选型: SQLite via `modernc.org/sqlite`(纯 Go 驱动,免 CGO)。**

- 数据量极小(几十个绑定 + 几千条短 TTL 缓存),SQLite 单文件绰绰有余。
- 纯 Go 驱动避免 CGO,交叉编译和容器化简单。
- **考虑过的替代方案**: BoltDB/bbolt(KV 够用但 SQL 查询和 migration 便利性差)、Postgres(过度工程)。
- Schema 最小化,两张表即可:

```sql
CREATE TABLE bindings (
  group_chat_id   INTEGER PRIMARY KEY,   -- 评论群 id (负数)
  channel_chat_id INTEGER NOT NULL,      -- 绑定频道 id (负数)
  bound_by_user_id INTEGER NOT NULL,     -- 执行 /bind 的创建者
  bound_at        INTEGER NOT NULL       -- unix seconds
);
CREATE INDEX bindings_channel_idx ON bindings(channel_chat_id);

CREATE TABLE verified_members (
  group_chat_id INTEGER NOT NULL,
  user_id       INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL,         -- unix seconds
  PRIMARY KEY (group_chat_id, user_id)
);
CREATE INDEX verified_expires_idx ON verified_members(expires_at);
```

- 内存侧再加一个小 LRU 做第二级缓存,避免每条消息都读 SQLite。进程重启时从 SQLite 预热。
- `verified_members` 的清理: 每小时扫一次删除 `expires_at < now`,不依赖索引即可。

### 消息决策流水线

单条 update 的处理路径(**顺序固定,短路返回**):

```
update 进入 → 按 chat.id 查 bindings,未绑定 → 忽略
             │
             ▼
           分类:
            ├─ service message / channel_post          → 忽略
            ├─ edited_message                          → 按下面同样规则走一遍,结论"删"则删掉编辑后版本
            ├─ message.sender_chat == bound_channel    → 放行(频道根帖)
            ├─ message.sender_chat != nil(其他频道身份)→ 删
            ├─ message.from.is_bot && in bot_allowlist → 放行
            ├─ message.from == GroupAnonymousBot       → 放行(可配置)
            └─ message.from 是普通用户:
                 ├─ 查 verified_members[group, user]
                 │    ├─ 命中且未过期 → 放行
                 │    └─ miss / 过期:
                 │         ├─ getChatMember(channel, user)
                 │         │    ├─ creator/administrator/member/restricted(restricted 但仍在群里视为 member)
                 │         │    │    → 写入 verified_members (expires_at = now + 30 min) → 放行
                 │         │    └─ left/kicked → 删
                 │         └─ getChatMember 错误:
                 │              ├─ 限速(429) → 排队重试 + 本条默认放行(避免静默)
                 │              └─ 其他错误 → 本条默认放行 + 告警日志
```

理由:

- 白名单短路放在网络调用之前,是 cost 最低的分类。
- `getChatMember` 错误时**默认放行**而不是默认删——删错了对用户伤害更大,放过广告被发现的成本是运营者可接受的。
- `restricted` 仍视为成员:有时频道主对老粉做了限制但没踢,依然算关注者。

### 媒体组(album)去重

同一 `media_group_id` 的 N 条消息,决策结果一定相同(同一 user、同一时刻)。
实现时用一个短 TTL(60s)的 `sync.Map[media_group_id]decision` 缓存**首条**决策,后续消息直接沿用,避免 N 次 `getChatMember`。

### `/bind` 的权限和参数

**只能在目标群内执行,无参数。**

- bot 调用 `getChat(group_id)`,从返回里取 `linked_chat_id`。若为 nil → 报错"此群未绑定频道,请先在频道设置中关联 linked discussion group"。
- bot 调用 `getChatMember(group_id, caller)`,要求返回 `creator`(精确相等)。否则拒绝。匿名管理员(`from.id == 1087968824`)在此规则下被自然拒绝,无需专门的判定分支。
- bot 调用 `getChatMember(channel_id, self)`,要求返回 `administrator`。否则提示"请先将我加为频道管理员"。
- 条件满足 → upsert 到 `bindings` 表,返回 "已绑定: 群 <group_title> ↔ 频道 <channel_title>"。

不让用户手填 channel id,安全边界完全由 `linked_chat_id` 决定——只有频道主能设置 linked group,所以绑错的风险被 Telegram 自己消掉了。

**考虑过的替代方案**: 支持 `/bind @channel_username` 手动绑定。拒绝是因为这会打开"把群绑到陌生频道"的攻击面(比如公共群里某个管理员把群绑到 scam 频道刷 KYC)。

### 运行时与并发模型

- 单进程 long polling,通过 `telego` 的 `UpdatesViaLongPolling`。
- 每条 update 交给一个 goroutine 处理(`go handle(update)`),主循环不阻塞。
- `context.Context` 全链路传递,shutdown 时 cancel,等待飞行中的 handler 退出(带超时 5s,超时则强退)。
- 每个 update handler 加 `defer recover()`,panic 不影响其他消息。
- Bot API 限速:使用一个 token bucket(30/s 全局 + 每 chat 1/s)包裹 SDK 调用。429 响应按 `retry_after` 回退。

### 可观测性

- 结构化日志(`log/slog` JSON handler),每条决策记录: `group_id`, `user_id`, `decision`, `reason`, `cache_hit`, `latency_ms`。
- `/status` 命令(仅群创建者可调用)输出:当前绑定、近 1h 删除计数、缓存命中率、最近 5 次 getChatMember 错误。
- 指标(可选,v1 可先只打日志):不引入 Prometheus,保持单二进制零依赖。后续如需,再加 `promhttp` handler。

### 配置

最小配置通过环境变量或 `config.yaml`:

```yaml
bot_token: "..."         # 必填
db_path: "./bot.db"      # 默认
cache_ttl: "30m"         # 默认
log_level: "info"
bot_allowlist: []        # 白名单 bot 的 user_id 列表
allow_anonymous_admin: true
```

环境变量优先于 yaml,方便容器部署。

## 风险 / 权衡

- **风险**: bot 被从频道移除管理员 → `getChatMember` 全部失败 → 当前策略是默认放行,评论群变成"全通过"。
  **缓解**: 定期(每 5 min)自检 bot 在已绑定频道的管理员身份,失效则在该群发告警消息"bot 已失去频道管理员权限,守卫已降级"。

- **风险**: 用户关注后被缓存 30min,期间退频道仍可发言。
  **缓解**: TTL 故意选短;文档明确"非实时";需要更严格的场景用户可自己改短 TTL。做精确实时反取关要订阅频道侧 `chat_member` 更新 + 维护反向索引,复杂度显著升高,v1 不做。

- **风险**: `getChatMember` 撞 429,短时大量消息默认放行。
  **缓解**: 全局 token bucket 平滑请求;429 时把待查用户放入短期"待决"队列,消息不立刻删也不立刻放行,等恢复后补判(实现成本较高,v1 先做默认放行 + 日志告警,v2 再补)。

- **风险**: 频道 `sender_chat` 匹配用的是 `channel_chat_id`,若频道被删重建、id 变了,所有绑定失效。
  **缓解**: `/bind` 时存一份 `channel_username`(若存在)作为二级标识;绑定自检失败时在 `/status` 里提示运营者重新 `/bind`。

- **权衡**: 选择"默认放行"而非"默认删",代价是 Telegram API 故障期间可能漏过广告。优先保护正常用户体验,这是产品方向上的明确取舍。

- **权衡**: 不做 webhook 模式,代价是需要 bot 进程有出站 HTTPS 能力、无法精确收到 `edited_channel_post` 等 long polling 默认不订阅的 update(用 `allowed_updates` 显式订阅 `message`、`edited_message`、`my_chat_member` 即可覆盖需要的)。

- **权衡**: SQLite 单文件,多实例部署会冲突。目前需求明确单实例,可接受;未来如要高可用,需要换存储后端并做 leader election。

## 迁移计划

项目从零起步,不涉及迁移。但 v1 → v2 的升级路径需要考虑:

- SQLite schema 用 `PRAGMA user_version` 标记版本号;启动时读版本号,按版本执行 `ALTER TABLE` / 增表。v1 初始化时设 `user_version = 1`。
- 绑定和缓存数据保留;bot token 若更换,缓存仍可用(user_id 稳定)。

回滚: 停旧进程 → 启动旧版二进制指向同一 SQLite 文件。前提是 schema 向前兼容(新版不破坏旧字段)。

## 待解问题

- `allow_anonymous_admin` 默认值确认 `true`——是否有场景需要强制匿名管理员也关注频道?留一个配置开关,v1 默认宽松。
- 是否需要 `/status` 的输出里包含频道关注数对比(通过 `getChatMemberCount` 查)?v1 先不做,避免额外 API 调用。
- 是否在**首次部署**时提供一个 `/selfcheck` 命令帮运营者验证 bot 权限配置正确?倾向于做,但放到 v1.1,不阻塞核心功能。
