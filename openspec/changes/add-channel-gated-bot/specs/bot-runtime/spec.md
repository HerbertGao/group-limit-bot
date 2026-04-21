## 新增需求

### 需求:系统必须通过长轮询接收 Telegram 更新

系统必须使用 Telegram Bot API 的 long polling(`getUpdates`)而非 webhook 接收 update;必须通过 `allowed_updates` 参数显式订阅 `message`、`edited_message` 与 `my_chat_member` 三种更新类型,不得订阅其他类型以减少流量;首次启动时必须调用 `deleteWebhook(drop_pending_updates=false)`;调用必须在最多 3 次、2 秒间隔的重试后仍失败时,进程必须以 FATAL 日志和非零状态码退出,不得在 webhook 可能仍激活的情况下进入长轮询(否则 Telegram 会继续推送到 webhook,导致静默失效)。

#### 场景:正常启动
- **当** bot 进程启动,读取到合法 `bot_token`
- **那么** 系统必须调用 `deleteWebhook` 一次,随后进入 `getUpdates` 长轮询循环,订阅列表恰好为 `["message", "edited_message", "my_chat_member"]`

#### 场景:deleteWebhook 持续失败
- **当** bot 启动时 `deleteWebhook` 连续 3 次调用均返回错误
- **那么** 进程必须以 FATAL 日志退出,不得进入长轮询循环

### 需求:每条 update 必须在独立 goroutine 中处理

主轮询循环收到 update 后必须立即将其分发至新 goroutine 处理,主循环不得等待单条消息处理完成;每个 handler goroutine 必须使用 `defer recover()` 捕获 panic,panic 必须以 ERROR 级别记录结构化日志,不得传播到主循环导致进程退出。runtime 必须对 in-flight update handler 并发度设置上限(默认 128),通过一个 buffered semaphore 实现;获取槽位必须 honor shutdown context 以避免关停期无限阻塞。这样 spam burst 不会导致 goroutine 无界累积。

#### 场景:某条消息 handler panic
- **当** 某条 update 在 handler 内触发 panic(如空指针解引用)
- **那么** 系统必须记录 ERROR 日志含 panic 值与堆栈,必须继续处理后续 update,必须不退出进程

#### 场景:高峰期 update handler 并发受限
- **当** 短时间内收到数百条 update,每条 handler 正在等 Telegram API
- **那么** 系统必须限制同时执行的 handler 数量不超过 128(通过 buffered semaphore);超出部分必须在 semaphore 上排队,不得无界 spawn goroutine

### 需求:所有 Telegram Bot API 调用必须受全局限速控制

系统必须通过一个进程内令牌桶包裹对 Telegram Bot API 的所有调用,令牌补给速率必须不高于 30 次/秒;若 API 返回 HTTP 429,系统必须读取响应中的 `parameters.retry_after`,在 `retry_after` 时间内暂停对该 chat 的新 API 调用(客户端层面阻塞直到时间到或 ctx 取消);在此期间的调用必须不立即失败,而是等待后继续,并在日志中记录限速事件。客户端的 5 秒超时只作用于 Telegram API 的实际往返调用,不包裹 `acquire` 阶段的等待;长 `retry_after`(例如 30 秒)期间 `acquire` 将等待而非超时失败,但 `ctx` 取消时立即返回。同一 chat 在 cooldown 窗口内收到第二个 429 时,pause 必须取两者中较晚的 deadline;较短的 `retry_after` 不得缩短已经生效的较长 backoff 窗口。

#### 场景:短时突发超过限速
- **当** 同一秒内产生超过 30 个 API 调用请求
- **那么** 超限请求必须在令牌桶中排队等待,必须不并发发出超过 30 req/s 的实际请求

#### 场景:收到 429 响应
- **当** 某次 `getChatMember` 调用收到 429 且 `retry_after = 5`
- **那么** 系统必须在后续 5 秒内不对该 chat 发起新的 API 调用,必须记录 WARN 日志

#### 场景:在 429 冷却窗口内发起新调用
- **当** 客户端对 chat C 的 getChatMember 调用刚收到 429 `retry_after = 5`,500ms 后又对同一 chat 发起新调用
- **那么** 新调用必须阻塞直到冷却窗口结束(约 4.5s)后再发出,不得立即返回 RateLimitError

#### 场景:同一 chat 并发收到两次 429
- **当** 同一 chat 在极短时间内先后收到 `retry_after=30s` 和 `retry_after=1s` 两个 429 响应
- **那么** 系统必须保留 30 秒的 pause deadline,不得被后到的 1 秒响应覆盖为较短窗口

### 需求:系统必须输出结构化日志

系统必须使用 `log/slog` 的 JSON handler 输出日志;每条决策日志必须至少包含字段 `group_id`、`user_id`、`decision`(`allow`/`delete`/`ignore`)、`reason`(`cache_hit`/`member`/`not_member`/`sender_chat_other`/`error_default_allow` 等)、`cache_hit`(bool)、`latency_ms`(int);日志级别必须通过配置项 `log_level` 控制(`debug`/`info`/`warn`/`error`)。

#### 场景:非成员消息被删除
- **当** 用户 U 在群 G 发消息被判定为非成员删除
- **那么** 系统必须输出一条 INFO 级别 JSON 日志,包含 `group_id=G`、`user_id=U`、`decision="delete"`、`reason="not_member"`、`cache_hit=false`

### 需求:系统必须支持优雅关停

进程必须监听 `SIGINT` 与 `SIGTERM`;收到信号后必须立即停止拉取新 update(通过取消 rootCtx),但 in-flight handler 的 context 必须独立于 rootCtx,以便在最多 5 秒的 grace 窗口内自然完成(删除消息、回复等 Telegram API 调用不得被立即中断)。5 秒超时后,系统必须显式取消 handler context 并记录 WARN 日志。

#### 场景:收到 SIGTERM
- **当** 进程正在处理 3 个 update,收到 SIGTERM
- **那么** 系统必须停止新 update 拉取,in-flight handler 必须继续持有其 context(未被立即取消),在 5 秒内完成则进程正常退出;否则必须在 5 秒整点显式取消 handler context 并记录"in-flight handlers were force-cancelled"的 WARN 日志

### 需求:配置必须可通过环境变量与 YAML 文件注入且环境变量优先

系统必须支持两种配置来源:`config.yaml` 文件(路径由启动参数或默认 `./config.yaml`)与环境变量;环境变量命名约定为 `BOT_<UPPER_SNAKE_KEY>`(如 `BOT_TOKEN`、`BOT_DB_PATH`);同名配置项若两处都提供,环境变量必须覆盖 YAML 值;`bot_token` 未提供时进程必须启动失败并输出 FATAL 日志,不得在无 token 情况下继续。白名单 bot 列表的规范名称为 YAML 键 `allowlist` 与环境变量 `BOT_ALLOWLIST`;旧名 `bot_allowlist` / `BOT_BOT_ALLOWLIST` 必须作为弃用别名继续接受一个版本周期,新旧两处同时存在时以新名为准。

#### 场景:YAML 与环境变量同时提供 token
- **当** `config.yaml` 含 `bot_token: A`,环境 `BOT_TOKEN=B`
- **那么** 系统必须使用 token `B`

#### 场景:未提供 token
- **当** 既无 YAML 也无环境变量提供 `bot_token`
- **那么** 进程必须以非零状态码退出并输出 FATAL 日志"bot_token is required"

### 需求:系统必须定期自检 bot 在绑定频道的管理员身份

系统必须每 5 分钟对所有绑定对执行双重健康检查:`getChatMember(channel, bot_id)` 返回 `administrator`,且 `GetChatMemberCanDelete(group, bot_id) == true`。若 `getChatMember` 或 `GetChatMemberCanDelete` 返回**非瞬态**错误(`context.DeadlineExceeded`、`context.Canceled`、`*RateLimitError` 以外的错误)或返回非 administrator / 无删除权限,系统必须视为降级并告警。瞬态错误必须只记录 WARN 日志,不得触发告警。告警内容必须合并指出所有失败维度(频道侧与群侧),并向对应绑定群发送;成功发送告警后,对同一群的再次告警必须节流至每小时最多一次;若告警发送失败,必须不登记节流,下一次 tick 必须重新尝试。若某一 tick 发现群已恢复健康,系统必须清除该群的 `lastWarned` 冷却状态,以便后续再次降级时能立即告警(否则会错过紧随恢复后发生的新故障)。清除 cooldown 只能在**两项检查都返回正向干净结果**(`getChatMember` 返回 administrator 且 `GetChatMemberCanDelete` 返回 true)时发生。若本次 tick 仅收到瞬态错误而没有任何确定性的 clean 结果,cooldown 必须保持原状,避免瞬态错误把节流状态重置成"可立即再次告警"。

#### 场景:频道管理员被撤销
- **当** 运营者手动将 bot 从频道 C 管理员降为普通成员
- **那么** 下一次自检必须检测到,并在对应的评论群发送一次告警消息;1 小时内若仍未恢复,必须不重复发送

#### 场景:首次告警发送失败时不得登记冷却
- **当** 自检检测到 bot 已失去频道 C 管理员权限,对群 G 发送告警消息返回错误
- **那么** 系统必须不登记 `lastWarned[G]`,下一次 5 分钟 tick 必须再次尝试发送

#### 场景:自检调用 getChatMember 返回错误
- **当** 自检调用 `getChatMember(channel, botID)` 收到错误(如 bot 已被移出频道导致的 403)
- **那么** 系统必须视为降级,必须向对应评论群发送告警消息(受 1h cooldown 保护),必须记录 WARN 日志

#### 场景:bot 在评论群内失去删除消息权限
- **当** bot 在绑定评论群 G 内被降级为非管理员或 can_delete_messages 被取消
- **那么** 下一次自检必须检测到并向 G 发送降级告警,告警内容必须指明群侧权限问题

#### 场景:自检遇到瞬态 Telegram 错误
- **当** 自检调用 `getChatMember` 或 `GetChatMemberCanDelete` 收到 `*RateLimitError` 或 `context.DeadlineExceeded`
- **那么** 系统必须只记录 WARN 日志,必须不将此视为降级,必须不向绑定群发送告警消息

#### 场景:告警后恢复再度降级必须重新告警
- **当** bot 对群 G 发送过降级告警,然后在下一次 tick 时一切正常(canDelete=true 且 channel admin),30 秒后又降级
- **那么** 第二次降级必须立即发送新告警,不得被之前的 1 小时冷却抑制

#### 场景:瞬态错误不得清除节流
- **当** bot 对群 G 发过告警后,下一次 tick 仅收到 `*RateLimitError`(未得到 channel admin / group delete 的任一 clean 结果)
- **那么** 系统必须不清除 `lastWarned[G]`;若紧接着的 tick 又看到真实降级,必须遵守原 1 小时 cooldown 不得重复告警

### 需求:内存侧 MemberCache 必须有容量上限

内存侧 MemberCache 必须有上限(默认 10000 条)。当达到上限且需写入新条目时,必须按 earliest-expiring 策略批量驱逐约 10% 条目,以防高流量下 cache 无界增长。SQLite L2 不受此上限约束。启动时的 `Prime` 阶段必须同样遵守该上限;若 SQLite 中已有超过上限的有效 verified_members 行,`Prime` 必须按 `expires_at` 降序选取 `maxEntries` 条加载,其余留给后续访问按需从 L2 命中获取。`Get` 在 SQLite 命中后回填内存时也必须执行同样的 cap + evict 检查;否则启动后的冷运行会绕过上限造成无界增长。

#### 场景:cache 写入达到上限
- **当** MemberCache 已存有 10000 条活跃条目,系统为一个新 `(group, channel, user)` 键调用 `Set`
- **那么** 系统必须按到期时间从早到晚驱逐约 1000 条(10%)后再写入新条目,必须不无界增长
