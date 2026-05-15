## 上下文

Telegram Bot API 10.0 的 Guest Mode 让广告 bot 无需入群即可被 `@` 召唤后投放广告。本变更基于一次完整的实测调查（curl 直打 Bot API，详见 `docs/guest-bots.md`），关键已验证事实：

- guest bot 的广告回复默认是 inline message，对第三方 bot 不可见。
- **守卫 bot 在 `@BotFather` 开启 Bot-to-Bot Communication Mode 后**（且为群管理员或 Privacy Mode 关闭），guest 回复会以普通 `message` update 进入 update 流。
- 该消息携带 `from`（广告 bot 本身）、`message_id` + `chat_id`、`reply_to_message`（指向召唤消息）、以及 `guest_bot_caller_user`（召唤者）。
- 守卫 bot（群管理员）对该消息调用 `deleteMessage` 成功，广告全群消失。
- 召唤由 Telegram 服务端即时触发，守卫 bot 永远在「事后」收到消息——无法预防第一条广告。

当前状态：`internal/gating` 采用「懒校验删除」模型，bot 白名单是进程级全局 `map[int64]bool`（启动时从 config 构建）。绑定关系存于 SQLite，命令 `/bind`、`/unbind`、`/status` 仅群创建者可用。

## 目标 / 非目标

**目标：**

- 收到未授权 guest-bot 广告后，最快删除广告本体及其召唤消息。
- 引入群级 bot 白名单，由群创建者用命令维护；生效白名单 = 全局 ∪ 群级。
- 对重复召唤未授权 guest bot 的用户做升级惩罚（禁言 → ban），不论其是否关注频道。

**非目标：**

- 预防某用户发出的「第一条」广告——结构上不可能（服务端即时触发）。
- L4「默认全员禁言」架构改造——与项目刻意选择的懒校验模型冲突，明确排除。
- L3「新成员入群禁言窗口」——本变更不做，留作未来可选增强。
- 在召唤消息阶段主动识别「这是一次 bot 召唤」——召唤消息对本 bot 只是带 `@` mention 的普通消息，无法可靠区分，不依赖它。

## 决策

**D1：用 `guest_bot_caller_user` / `guest_bot_caller_chat` / `guest_query_id` 作确定性判据。** guest 回复消息只要带这三个字段任一即可确定为 guest 回复（实测第三方收到的、由用户召唤的回复带 `guest_bot_caller_user`，频道/匿名召唤则为 `guest_bot_caller_chat`）。该检查必须排在决策流水线最前部——在绑定查找与服务消息判定之后、在 `sender_chat` 分支与 `getChatMember` 之前,以防带 `sender_chat` 的 guest 回复被 `channel_root_post` / `other_sender_chat` 分支误判。
- 备选：仅靠现有路径——guest bot 的 `from.IsBot=true`、非频道成员、非白名单 → `getChatMember` 查不到成员 → 删。问题：`getChatMember` 对一个 bot 的 user id 查询行为未验证，若报错则走 `ReasonErrorDefaultAllow` fail-open，广告漏网。确定性字段零 API 成本、无 fail-open。

**D2：部署前提为 Bot-to-Bot Communication Mode。** 无替代方案；不开启则 guest 回复完全不可见。写入 README 与部署文档，与「关闭 Privacy Mode」并列。

**D3：依赖处理优先升级 telego。** `telego v1.8.0` 无 `guest_*` 字段。优先升级到含 Bot API 10.0 字段的 telego 版本；若暂无可用 release，则需 raw-JSON 解码 `guest_bot_caller_user` / `guest_bot_caller_chat` / `guest_query_id`——注意 telego 的 `UpdatesViaLongPolling` 返回的是已类型化的 `telego.Update`、不保留原始 JSON，raw-JSON 方案需在 HTTP 层拦截或改造长轮询路径取得原始 update 字节，属架构性改动而非简单兜底，须在 task 1.1 调研阶段一并设计该拦截点。

**D4：群级白名单新建独立表，与全局并存。** 新表 `group_bot_allowlist(group_chat_id, bot_user_id, bot_username, added_by, added_at)`，主键 `(group_chat_id, bot_user_id)`。gate 判定 = 全局 `map` ∪ 该群的群级集合。全局仍由 config 注入（运营者进程级），群级由命令动态增删（各群创建者）。

**D5：群级白名单按 `@username` 添加。** `/allowbot @bot` 经 `getChat` 解析为数字 id 存库；`bot_username` 仅供展示，可能过期，`bot_user_id` 为权威键（bot 改名不影响匹配）。不强校验 `is_bot`——gate 仅在 `from.IsBot` 时查白名单，误加非 bot id 为无害死数据。可选支持「回复某 bot 消息发 `/allowbot`」，此时从 `reply_to_message.from` 直取 id。

**D6：L1 单 update 同时删广告 + 召唤。** guest 回复 update 自带 `reply_to_message.message_id`（即召唤消息 id），无需自行存储召唤消息即可一并删除。快删同时堵掉「回复旧广告二次召唤」入口。guest 回复结构上恒为单条消息（`answerGuestQuery` 仅接受单个 `InlineQueryResult`，且每次召唤仅允许一次回复），不存在 media-group 分条情形。

**D7：召唤者升级惩罚，违规计数持久化。** 依 `guest_bot_caller_user` 定位召唤者；store 中按 `(group_chat_id, user_id)` 记违规次数。首次仅删除；达阈值禁言（`restrictChatMember`, `can_send_messages=false`）；再达阈值 ban。阈值与禁言时长作配置项，给默认值。仅当被召唤 bot **不在**生效白名单时才计违规——白名单内的 guest bot 既不删也不罚。`guest_bot_caller_user` 缺失（召唤者为频道/匿名身份）时只删除、不计违规、不惩罚。违规计数单调累加、不随时间衰减；衰减作为未来可选增强，本变更不做。对管理员/创建者召唤者的 `restrictChatMember`/ban 会失败，此类错误必须被容忍并记 WARN 日志。违规计数每次自增后再与阈值比较，且只在 guest 回复首次以 `message` 到达时累加（`edited_message` 重复到达不重复计数）。禁言阈值最小有效值为 2、封禁阈值必须严格大于禁言阈值，非法配置必须启动失败，以确保「首次违规恒不罚」。

**D8：群级白名单内存缓存。** 按 `group_chat_id` 懒加载到内存 map，`/allowbot`、`/disallowbot` 变更时失效，避免每条消息打 DB。

**D9：`/unbind` 清理。** 解绑时一并删除该群的 `group_bot_allowlist` 条目与违规计数，与「解绑清缓存」一致。

**D10：列表并入 `/status`。** 不新增列表命令，`/status` 输出附带本群群级白名单（bot 用户名/id 列表）。

## 风险 / 权衡

- [无法预防第一条广告] → 接受；目标定为压缩闪现时间（约 1 秒内删除）+ 阻止重复。与 Telegram 自身 Aggressive 反垃圾同为事后删，属 Bot API 能力上限。
- [`getChatMember` 对 bot id 的查询行为未单独验证] → 用 D1 确定性字段规避，不依赖该路径。
- [本次仅在基础 `group` 验证，真实评论区为 `supergroup`] → 机制相同，预期一致；列入迁移计划，上线前在 supergroup 复验。
- [开启 Bot-to-Bot 模式后收到群内所有 bot 消息，update 量上升] → 可接受；现有并发处理（`updateHandlerConcurrency`）足以承载。
- [bot-to-bot 死循环] → 官方有 Loop Prevention 警告；本 bot 只删除、不回复 bot，不构成循环；删除仍受 Telegram 限速约束，沿用现有 executor 限速处理。
- [误伤正常 guest 工具] → 生效白名单豁免；惩罚仅针对召唤了**未授权** guest bot 的用户。
- [惩罚误伤] → 首次不惩罚仅删除；禁言时长、ban 阈值可配，给保守默认值。
- [telego 升级引入 breaking changes] → 升级前评估；raw-JSON 解码作为后备方案。

## 迁移计划

- **部署**：运营者在 `@BotFather` 为守卫 bot 开启 Bot-to-Bot Communication Mode。README 部署步骤新增此项。
- **数据**：进程启动时自动建表 `group_bot_allowlist` 及违规计数表（沿用现有 schema 初始化方式）。
- **配置**：新增惩罚相关配置项（禁言阈值、禁言时长、ban 阈值），均带默认值，缺省即可运行。
- **回滚**：删除新表、关闭 Bot-to-Bot 模式即可恢复旧行为，无破坏性变更。
- **上线前**：在一个真实 supergroup 评论区复验 guest 回复可见性与删除。

## 待解决问题

- telego 是否已有包含 Bot API 10.0 `guest_*` 字段的 release；若无，确定 raw-JSON 解码的接入点。
- 惩罚阈值与禁言时长的具体默认数值。
