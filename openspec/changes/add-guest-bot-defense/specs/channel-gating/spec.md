## 新增需求

### 需求:未授权 guest-bot 回复及其召唤消息必须被删除

当守卫 bot 在 `@BotFather` 开启 Bot-to-Bot Communication Mode 后,Telegram guest bot 的回复会以普通 `message` update 进入决策流水线。系统必须将携带 `guest_bot_caller_user`、`guest_bot_caller_chat` 或 `guest_query_id` 任一字段的消息识别为 guest-bot 回复。该识别与处理必须排在决策流水线的最前部 —— 在绑定查找与服务消息判定之后,在 `sender_chat` 分支判定、白名单短路与 `getChatMember` 之前;一条带上述任一字段的消息即便同时带有 `sender_chat`(无论其值是否等于绑定频道),也必须按本需求处理,禁止落入 `channel_root_post` 放行分支或 `other_sender_chat` 分支。被编辑的 guest-bot 回复(`edited_message`)必须同样走本识别与删除逻辑。对于这类消息,若发送方 bot(`message.from.id`)不在生效白名单(全局白名单 ∪ 当前绑定群的群级 bot 白名单)内,系统必须调用 `deleteMessage` 删除该回复;若该回复带有 `reply_to_message`,系统必须一并删除 `reply_to_message`(即召唤消息),删除召唤消息时若其已不存在(例如已被门禁流水线删除),`deleteMessage` 返回的"消息不存在"类错误必须被容忍,不得视为处理失败。若发送方 bot 在生效白名单内,系统必须放行,禁止删除、禁止计违规。

#### 场景:未授权 guest bot 广告被删除
- **当** 评论群 G 收到一条带 `guest_bot_caller_user` 字段的消息,发送方 bot 不在 G 的生效白名单内
- **那么** 系统必须删除该消息

#### 场景:召唤消息一并删除
- **当** 一条未授权 guest-bot 回复带有 `reply_to_message`(指向召唤者发出的召唤消息)
- **那么** 系统必须同时删除该回复与 `reply_to_message` 所指的召唤消息

#### 场景:白名单内的 guest bot 放行
- **当** 评论群 G 收到带 `guest_query_id` 字段的消息,发送方 bot 的 user_id 在 G 的生效白名单内
- **那么** 系统必须放行该消息,禁止删除,禁止计违规

#### 场景:带 sender_chat 的 guest 回复
- **当** 一条带 `guest_query_id` 的消息同时带有 `sender_chat`(其值等于绑定频道或任意其他频道),发送方 bot 不在生效白名单内
- **那么** 系统必须按本需求删除该消息,禁止落入 `channel_root_post` 放行分支

#### 场景:召唤消息已被门禁删除
- **当** 删除一条未授权 guest 回复时,其 `reply_to_message` 已被门禁流水线删除
- **那么** 系统对召唤消息的 `deleteMessage` 收到"消息不存在"错误时必须容忍,不得视为处理失败

### 需求:召唤未授权 guest bot 的用户必须被升级惩罚

系统必须以 `(group_chat_id, 召唤者 user_id)` 为键持久化记录每个用户召唤未授权 guest bot 的违规次数,召唤者身份取自 guest-bot 回复消息的 `guest_bot_caller_user`。若该回复缺失 `guest_bot_caller_user`(例如召唤者为频道/匿名身份,仅带 `guest_bot_caller_chat`),系统必须仍删除该回复,但禁止累加任何违规计数、禁止施加惩罚。违规计数必须在每次未授权召唤被处理时先自增 1、再与阈值比较。违规计数只在 guest 回复首次以 `message` 形式到达时累加;同一 guest 回复以 `edited_message` 形式重复进入流水线时,系统必须删除该消息,但禁止再次累加违规计数。首次违规(计数为 1)系统必须只删除消息、禁止施加惩罚,该保证与阈值配置无关。当某召唤者的违规次数达到配置的禁言阈值,系统必须调用 `restrictChatMember` 对其禁言(`can_send_messages = false`),禁言时长由配置项决定。当违规次数达到配置的封禁阈值,系统必须 ban 该召唤者。禁言阈值配置项的最小有效值为 2,封禁阈值必须严格大于禁言阈值;配置不满足该约束时进程必须启动失败。违规计数单调累加、不随时间衰减(衰减为未来可选增强)。该判定必须不依赖召唤者是否关注绑定频道。若对召唤者执行 `restrictChatMember` 或 ban 因其为群管理员/创建者而失败,系统必须容忍该错误并记 WARN 日志,不得视为处理失败。被召唤 bot 在生效白名单内时,系统必须不计违规、不施加惩罚。

#### 场景:首次召唤只删不罚
- **当** 用户 U 首次在群 G 召唤了一个未授权 guest bot
- **那么** 系统必须删除广告与召唤消息,禁止禁言或封禁 U

#### 场景:重复召唤达阈值被禁言
- **当** 用户 U 在群 G 的违规次数达到配置的禁言阈值
- **那么** 系统必须调用 `restrictChatMember` 按配置时长禁言 U

#### 场景:已关注频道的召唤者同样被惩罚
- **当** 用户 U 是绑定频道的成员,且其在群 G 的违规次数达到禁言阈值
- **那么** 系统必须照常禁言 U,关注状态不得豁免惩罚

#### 场景:召唤白名单内 bot 不计违规
- **当** 用户 U 召唤的 guest bot 在群 G 的生效白名单内
- **那么** 系统必须不增加 U 的违规计数,禁止施加惩罚

#### 场景:召唤者非用户身份
- **当** 一条未授权 guest 回复缺失 `guest_bot_caller_user`
- **那么** 系统必须删除该回复,禁止累加任何违规计数,禁止施加惩罚

#### 场景:对管理员召唤者的惩罚失败被容忍
- **当** 召唤未授权 guest bot 的用户是群管理员/创建者,其违规次数达到禁言或封禁阈值,`restrictChatMember`/ban 因目标为管理员而失败
- **那么** 系统必须容忍该错误并记 WARN 日志,不得视为处理失败

#### 场景:违规计数自增后比较阈值
- **当** 禁言阈值为 N,某召唤者第 N 次召唤未授权 guest bot
- **那么** 系统必须在该次处理时把其计数自增到 N、判定达阈值并禁言

#### 场景:被编辑的 guest 回复不重复计数
- **当** 一条已计过违规的 guest 回复随后以 `edited_message` 再次进入流水线
- **那么** 系统必须删除该消息,但禁止再次累加该召唤者的违规计数

## 修改需求

### 需求:白名单身份必须短路放行

系统必须在调用 `getChatMember` 之前识别以下发送者并直接放行,以降低 API 调用:bot 自身、生效白名单中列出的 bot、`message.from.id == 1087968824`(Telegram `GroupAnonymousBot`,代表群匿名管理员,当 `allow_anonymous_admin == true` 时放行)。**生效白名单必须为「全局白名单配置项」与「当前绑定群的群级 bot 白名单(`group_bot_allowlist` 表中 `group_chat_id` 等于当前群的条目)」的并集;某 bot 的 user_id 出现在任一份白名单中即视为命中。** 群级白名单必须以 `group_chat_id` 为维度隔离,一个群的群级白名单条目不得用于另一个群的判定。

#### 场景:匿名管理员发消息且配置允许
- **当** 评论群 G 收到 `from.id == 1087968824` 的消息,且配置 `allow_anonymous_admin == true`
- **那么** 系统必须放行,不调用 `getChatMember`

#### 场景:匿名管理员以群身份发言
- **当** 评论群 G 收到 `sender_chat.id == G 本身` 与 `from.id == 1087968824` 的消息,配置 `allow_anonymous_admin == true`
- **那么** 系统必须放行,必须不调用 `getChatMember`,必须不删除

#### 场景:全局白名单 bot 发消息
- **当** 某 bot B 的 user_id 在全局白名单配置项中,并在评论群 G 发消息
- **那么** 系统必须放行

#### 场景:群级白名单 bot 发消息
- **当** 某 bot B 的 user_id 不在全局白名单,但在评论群 G 的 `group_bot_allowlist` 条目中,并在 G 发消息
- **那么** 系统必须放行 B 在群 G 的消息

#### 场景:群级白名单按群隔离
- **当** bot B 在群 G1 的 `group_bot_allowlist` 中,但不在群 G2 的 `group_bot_allowlist` 中,也不在全局白名单中,B 在群 G2 发消息
- **那么** 系统必须不因白名单短路放行 B 在 G2 的消息
