## 1. 依赖与字段解码

- [x] 1.1 调研 telego 是否已有包含 Bot API 10.0 `guest_*` 字段的 release;有则升级 `go.mod`
- [x] 1.2 若无可用 telego release:设计 raw-update 拦截点 —— `UpdatesViaLongPolling` 返回已类型化的 `telego.Update`、不保留原始 JSON,需在 HTTP 层拦截或改造长轮询路径取得原始字节(架构性改动,非简单兜底)
- [x] 1.3 让 `internal/telegram` 能从收到的 `message` 与 `edited_message` 中取得 `guest_bot_caller_user`、`guest_bot_caller_chat`、`guest_query_id`、`reply_to_message`(升级后直接用结构体;否则按 1.2 的拦截点对这几个字段做 raw-JSON 解码)

## 2. 数据层

- [x] 2.1 `internal/store`:新增 `group_bot_allowlist` 表(主键 `(group_chat_id, bot_user_id)`,字段 `bot_username`/`added_by`/`added_at`),启动时自动建表
- [x] 2.2 `internal/store`:实现 `group_bot_allowlist` 的 CRUD —— 新增(幂等)、按群删除单条、按群列出、按 `(group, bot_user_id)` 查询
- [x] 2.3 `internal/store`:新增 guest 召唤违规计数表(键 `(group_chat_id, user_id)`),启动时自动建表
- [x] 2.4 `internal/store`:实现违规计数的累加与读取
- [x] 2.5 `internal/store`:扩展 `DeleteBinding`,在删除 bindings 行时级联清理该群的 `group_bot_allowlist` 与违规计数
- [x] 2.6 store 层单元测试(建表、CRUD、按群隔离、级联清理、重启后恢复)

## 3. 配置

- [x] 3.1 `internal/config`:新增惩罚配置项(禁言阈值、禁言时长、封禁阈值),提供保守默认值;启动时校验禁言阈值 ≥ 2 且封禁阈值 > 禁言阈值,非法配置必须启动失败
- [x] 3.2 config 加载与默认值的单元测试

## 4. 群级白名单接入 gate

- [x] 4.1 实现群级白名单查询服务:store 支撑 + 按 `group_chat_id` 懒加载的内存缓存
- [x] 4.2 `/allowbot`、`/disallowbot` 变更时使对应群的缓存失效
- [x] 4.3 `internal/gating`:把 `decideBase` 的 bot 白名单短路从「仅全局」改为「全局 ∪ 当前群群级白名单」
- [x] 4.4 gate 白名单短路的单元测试(全局命中、群级命中、按群隔离不命中)
- [x] 4.5 `/unbind` 解绑时,除级联清表(见 2.5)外,必须使该群的群级白名单内存缓存失效

## 5. guest-bot 检测与删除(L1)

- [x] 5.1 `internal/gating`:在决策流水线最前部(绑定查找与服务消息判定之后、`sender_chat` 分支与 `getChatMember` 之前)识别带 `guest_bot_caller_user`/`guest_bot_caller_chat`/`guest_query_id` 任一字段的消息为 guest-bot 回复
- [x] 5.2 未授权(发送 bot 不在生效白名单)的 guest 回复 → 判定删除;白名单内 → 放行、不计违规
- [x] 5.3 删除广告本体的同时,删除其 `reply_to_message`(召唤消息);召唤消息已被删除时容忍 `deleteMessage` 的"消息不存在"错误
- [x] 5.4 guest 检测与删除的单元测试(未授权删除、召唤消息一并删、白名单豁免)
- [x] 5.5 流水线顺序测试:带 `sender_chat`(含等于绑定频道)的 guest 回复必须被识别删除、不落入 `channel_root_post` 分支;`edited_message` 形态的 guest 回复同样被处理

## 6. 召唤者升级惩罚(L2)

- [x] 6.1 `internal/gating`:删除未授权 guest 回复时,按 `guest_bot_caller_user` 累加召唤者违规计数(计数先自增再比阈值;只在首次以 `message` 到达时累加,`edited_message` 重复到达不重复计数)
- [x] 6.2 实现阈值判定:首次只删不罚;达禁言阈值 → `restrictChatMember` 按配置时长禁言;达封禁阈值 → ban
- [x] 6.3 确认判定不依赖召唤者的频道关注状态
- [x] 6.4 处理 `guest_bot_caller_user` 缺失(召唤者为频道/匿名身份)→ 只删不计违规;`restrictChatMember`/ban 对管理员召唤者失败 → 容忍并记 WARN 日志
- [x] 6.5 升级惩罚的单元测试(首次不罚、达阈值禁言、达阈值 ban、关注者同样受罚、召唤者非用户不计违规、对管理员惩罚失败被容忍、计数自增后比阈值、`edited_message` 不重复计数、非法阈值配置启动失败)

## 7. 管理员命令

- [x] 7.1 实现 `/allowbot` handler:校验群创建者;解析参数 `@username`(`getChat` → user_id);写 `group_bot_allowlist`;已存在则幂等回复;解析失败回复错误
- [x] 7.2 (可选)支持回复某 bot 消息执行 `/allowbot`,从 `reply_to_message.from` 直取 id
- [x] 7.3 实现 `/disallowbot` handler:校验群创建者;从 `group_bot_allowlist` 删除当前群对应条目
- [x] 7.4 把 `/allowbot`、`/disallowbot` 注册进 dispatcher
- [x] 7.5 扩展 `/status`:输出当前群 `group_bot_allowlist` 各条目的 `bot_username` 与 `bot_user_id`
- [x] 7.6 命令的单元测试(创建者增删、幂等、非创建者拒绝、解析失败、`/status` 含白名单)

## 8. 文档与验证

- [x] 8.1 `README.md`:部署步骤新增「`@BotFather` 开启 Bot-to-Bot Communication Mode」,与「关闭 Privacy Mode」并列
- [x] 8.2 `README.md`:补充 `/allowbot`、`/disallowbot` 命令说明与新增配置项
- [ ] 8.3 在一个真实 supergroup 评论区复验:guest 回复可见、删除成功、召唤者惩罚生效
