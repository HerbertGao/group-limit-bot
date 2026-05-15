## 为什么

Telegram Bot API 10.0（2026-05-08）推出 **Guest Mode**：广告 bot 无需加入群组、无需任何权限，被人工 `@` 召唤后即可往群里投放广告。它绕开了本 bot「关注绑定频道才能发言」的门禁——广告本体由一个非群成员的 guest bot 发出，召唤者哪怕没关注频道也能触发。经实测确认，这已成为 Telegram 评论群广告的主要新形态。若本 bot 无法识别并清除 guest bot 广告，评论群门禁将形同虚设。

## 变更内容

- 新增 **guest-bot 广告删除**：开启 Bot-to-Bot Communication Mode 后，guest bot 的回复以普通 `message` 进入 update 流。凡带 `guest_bot_caller_user` / `guest_bot_caller_chat` / `guest_query_id` 字段、且发送方 bot 不在白名单的消息一律删除，并顺带删除其 `reply_to_message`（即召唤消息）。
- 新增 **召唤者升级惩罚**：依 `guest_bot_caller_user` 定位召唤者，记录其违规次数；重复召唤未授权 guest bot 者先禁言、再升级为 ban。判定**不看**召唤者是否关注频道。
- 新增 **群级 bot 白名单**：每对（频道, 群）可由群创建者用命令维护一份允许的 bot 名单。最终生效白名单 = 全局 `allowlist` ∪ 当前群的群级白名单。
- 新增命令 `/allowbot @bot`、`/disallowbot @bot`（仅群创建者）；`/status` 输出附带本群的群级白名单。
- 修改 **bot 白名单语义**：gate 中对 bot 发言的白名单短路，从「仅查全局 allowlist」改为「查 全局 ∪ 当前群的群级白名单」。

## 功能 (Capabilities)

### 新增功能

无新增能力——本变更的所有行为都落在下列现有能力的范围内。

### 修改功能

- `channel-gating`: 新增针对 guest-bot 回复的删除规则；bot 白名单短路由「仅全局」改为「群感知（全局 ∪ 群级）」。
- `admin-commands`: 新增 `/allowbot`、`/disallowbot` 命令及其群创建者权限校验；`/status` 输出扩展为包含群级 bot 白名单。
- `group-binding`: 新增群级 bot 白名单的持久化（新数据表）；`/unbind` 解绑时一并清理该群的白名单条目。

## 影响

- **代码**: `internal/gating`（新增 guest-bot 判据、群级白名单查询、惩罚动作）、`internal/commands`（`/allowbot`、`/disallowbot`、`/status` 扩展）、`internal/store`（新表及 CRUD）。
- **依赖**: `telego v1.8.0` 源码无 `guest_*` 字段（早于 Bot API 10.0）——需升级 telego，或对 `guest_bot_caller_user` / `guest_bot_caller_chat` / `guest_query_id` 做 raw-JSON 解码。
- **部署**: 守卫 bot 必须在 `@BotFather` 开启 **Bot-to-Bot Communication Mode**（新前置条件，与「关闭 Privacy Mode」并列）。不开启则 guest 回复对本 bot 完全不可见、无法拦截。
- **数据**: SQLite 新增 `group_bot_allowlist` 表。
- **行为**: 开启 Bot-to-Bot Communication Mode 后，本 bot 会收到群内所有其他 bot 的消息，update 量上升（可接受）。
- **能力上限**: 无法预防某人发出的「第一条」广告（召唤由 Telegram 服务端即时触发）。本变更的目标是把广告闪现时间压到最短（约 1 秒内删除）并阻止同一召唤者重复作恶。
- **变更顺序依赖**: 本变更修改 `channel-gating`、`admin-commands`、`group-binding` 三个能力，其基线规范来自尚未归档的 `add-channel-gated-bot` 变更。本变更必须在 `add-channel-gated-bot` 归档之后才能应用与归档，否则 MODIFIED 需求没有基线可套、OpenSpec 校验会失败。
