## 1. 项目脚手架

- [x] 1.1 初始化 Go module: `go mod init github.com/herbertgao/group-limit-bot`,设置 Go 版本为 1.22+
- [x] 1.2 创建目录骨架: `cmd/bot/`、`internal/config/`、`internal/store/`、`internal/telegram/`、`internal/binding/`、`internal/gating/`、`internal/commands/`、`internal/runtime/`
- [x] 1.3 添加依赖: `github.com/mymmrac/telego` 与 `modernc.org/sqlite`;运行 `go mod tidy`
- [x] 1.4 创建 `Makefile` 含 `build`、`run`、`test`、`lint` 目标
- [x] 1.5 创建 `.gitignore` 忽略 `bot.db`、`config.yaml`、二进制产物
- [x] 1.6 创建 `README.md` 含最小化部署说明(bot token 如何获取、Privacy Mode 如何关闭、管理员权限要求)

## 2. 配置加载

- [x] 2.1 定义 `internal/config.Config` 结构体,字段含 `BotToken`、`DBPath`、`CacheTTL`、`LogLevel`、`BotAllowlist`、`AllowAnonymousAdmin`
- [x] 2.2 实现 YAML 加载(从 `./config.yaml` 或 `--config` 参数指定)
- [x] 2.3 实现环境变量覆盖(`BOT_<UPPER_SNAKE>` 命名);环境变量优先级高于 YAML
- [x] 2.4 `bot_token` 缺失时进程以 FATAL 日志和非零退出码终止
- [x] 2.5 为配置加载写单元测试,覆盖"仅 YAML"、"仅环境变量"、"两者冲突时环境变量优先"、"缺失 token"四场景

## 3. 存储层 (SQLite)

- [x] 3.1 `internal/store` 定义 `Store` 接口,包装 `*sql.DB`
- [x] 3.2 启动时执行 schema migration: `bindings` 表、`verified_members` 表,设置 `PRAGMA user_version = 1`
- [x] 3.3 实现 `Bindings` 方法: `Upsert`, `Delete`, `Get(groupID)`, `List()`
- [x] 3.4 实现 `VerifiedMembers` 方法: `Get(groupID, userID)`, `Upsert(groupID, userID, expiresAt)`, `DeleteExpired()`, `DeleteByGroup(groupID)`
- [x] 3.5 后台启动定时任务每 1 小时调用 `DeleteExpired()` 清理过期条目
- [x] 3.6 为 Store 方法写集成测试(使用 `:memory:` SQLite),覆盖 upsert 冲突、级联删除、过期清理

## 4. Telegram 客户端封装

- [x] 4.1 `internal/telegram` 封装 `telego.Bot`,暴露应用层需要的方法: `GetChatMember`, `DeleteMessage`, `SendMessage`, `GetChat`, `DeleteWebhook`
- [x] 4.2 实现全局令牌桶限速(30 req/s),所有 API 调用前必须获取令牌
- [x] 4.3 429 响应处理: 读取 `parameters.retry_after`,暂停对应 chat 的 API 调用至少 `retry_after` 秒,记录 WARN 日志
- [x] 4.4 所有方法接受 `context.Context`,超时默认 5 秒,支持 cancellation
- [x] 4.5 为客户端写 mock 供上层测试使用

## 5. 绑定管理 (group-binding)

- [x] 5.1 `internal/binding` 实现 `BindService`,依赖 Store 与 Telegram 客户端
- [x] 5.2 实现 `Bind(ctx, groupID, callerUserID) (*Binding, error)`:
  - 校验 chat 类型是 group/supergroup
  - 校验 caller 是群 creator/administrator
  - 从 `getChat(groupID).LinkedChatID` 取频道 id,为空则报错"未关联讨论频道"
  - 校验 bot 在频道中是 administrator
  - upsert 到 bindings 表,返回结构体含 `wasCreated bool`
- [x] 5.3 实现 `Unbind(ctx, groupID, callerUserID) error`:权限校验同 Bind;删除 bindings 行 + 清空该群的 verified_members
- [x] 5.4 实现 `Lookup(groupID) *Binding` 用于 gating 快速路径,miss 返回 nil
- [x] 5.5 为 BindService 写单元测试,mock Telegram 客户端,覆盖:正常绑定、非管理员拒绝、无 linked_chat_id 拒绝、bot 非频道管理员拒绝、覆盖已有绑定

## 6. 校验核心 (channel-gating)

- [x] 6.1 `internal/gating` 定义 `Decision` 枚举(`Allow`, `Delete`, `Ignore`)与 `Reason` 字符串常量
- [x] 6.2 实现 `Gate.Decide(ctx, msg) (Decision, Reason)`,短路顺序严格按 design.md 流水线
- [x] 6.3 实现 `verified_members` 的两级缓存: 进程启动时从 SQLite 预热到内存 LRU;读优先内存,写同时写内存和 SQLite
- [x] 6.4 实现 `media_group_id` 决策去重(`sync.Map`,TTL 60s,用 `time.AfterFunc` 清理)
- [x] 6.5 `getChatMember` 错误(429、超时、其他)一律返回 `Decision.Allow` + 相应 Reason,并记录 WARN 日志
- [x] 6.6 白名单短路顺序:service message → edited_message(递归走 Decide)→ sender_chat 分流 → bot 白名单 → anonymous admin → 普通用户校验
- [x] 6.7 为 Gate 写表驱动单元测试,至少覆盖 spec 中每个场景一份用例
- [x] 6.8 实现 `Executor`:接收 Decision 后调用 `DeleteMessage` 或放行,记录结构化决策日志

## 7. 命令分发 (admin-commands)

- [x] 7.1 `internal/commands` 实现 `Dispatcher`,根据 message 首词路由到 handler
- [x] 7.2 识别 `/command` 与 `/command@bot_username` 两种形式;bot_username 大小写不敏感匹配;指向其他 bot 则忽略
- [x] 7.3 所有 handler 先校验 caller 是群管理员(调用 `getChatMember`),非管理员统一回复权限错误
- [x] 7.4 `/bind` handler 调用 `BindService.Bind`,成功时回复 MarkdownV2 中文消息,标题做转义
- [x] 7.5 `/unbind` handler 调用 `BindService.Unbind`,未绑定时回复说明文案
- [x] 7.6 `/status` handler 聚合: 当前 binding、`verified_members` 计数、近 1h 删除计数(从 metrics 组件读)、最近 5 次 getChatMember 错误(从 error ring buffer 读),MarkdownV2 格式化输出
- [x] 7.7 命令消息必须被 gating 流水线跳过(在 Decider 短路:`text` 以 `/` 开头且匹配已注册命令时 `Ignore`)
- [x] 7.8 为 Dispatcher 与三个 handler 写单元测试,mock BindService

## 8. 运行时 (bot-runtime)

- [x] 8.1 `internal/runtime` 启动流程: 加载 config → 打开 Store → 构造 Telegram 客户端 → `deleteWebhook()` 一次 → 启动 long polling,`allowed_updates=["message","edited_message","my_chat_member"]`
- [x] 8.2 主循环每收到 update,`go` 启动 handler goroutine;handler 内 `defer recover()` 记录 panic 堆栈
- [x] 8.3 handler 顶层分流:命令消息走 Dispatcher,其余走 Gate → Executor
- [x] 8.4 监听 `SIGINT`/`SIGTERM`,触发 context cancellation 停止拉取,等待 in-flight handler 最多 5s,超时记录 WARN 后强退
- [x] 8.5 实现指标收集: 内存中 per-group 近 1h 删除滑动窗口计数(ring buffer),近 5 次 getChatMember 错误 ring buffer
- [x] 8.6 实现结构化日志 (`log/slog` JSON handler),日志级别由 config 控制
- [x] 8.7 实现频道管理员身份自检任务: 每 5 分钟遍历所有 bindings,`getChatMember(channel, bot)` 非 administrator 时在对应群发送告警消息,按 group_id 节流至每小时最多一次

## 9. 入口

- [x] 9.1 `cmd/bot/main.go` 组装所有组件,调用 `runtime.Run(ctx)`,退出码与错误传播正确
- [x] 9.2 支持 `--config <path>` flag 指定配置文件位置
- [x] 9.3 `go build ./cmd/bot -o bin/group-limit-bot` 能产出可运行的单二进制

## 10. 验证

- [x] 10.1 单元测试通过:`go test ./...`
- [ ] 10.2 手工集成测试: 创建测试频道 + 测试评论群 + 测试 bot,完成 `/bind`,用"已关注者"与"未关注者"两个账号各发一条消息,验证后者被删、前者保留
- [ ] 10.3 手工集成测试: 编辑"已关注者"的消息为包含链接,随后将该账号从频道移除,再次编辑,验证编辑后版本被删
- [ ] 10.4 手工集成测试: 以第三方频道身份发言,验证被删
- [ ] 10.5 手工集成测试: 撤销 bot 的频道管理员权限,等待 5 分钟,验证群内收到降级告警
- [ ] 10.6 压力测试: 用脚本向测试群投递 100 条消息混合 50 成员+50 非成员,验证所有非成员消息被删、成员消息全部保留、过程中无 panic、日志中 429 数量在预期内
- [x] 10.7 `openspec-cn validate add-channel-gated-bot --strict` 通过
