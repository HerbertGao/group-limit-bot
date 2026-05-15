## 新增需求

### 需求:未关注绑定频道的用户消息必须被删除

对于绑定评论群中由普通用户发送的消息(`message.from` 为非 bot 的真实用户),系统必须先通过 Telegram Bot API `getChatMember` 判定该用户在绑定频道的成员状态;若状态为 `left` 或 `kicked`,系统必须调用 `deleteMessage` 静默删除该条消息,不得向用户发送任何提示或反馈。

#### 场景:非成员发普通文本消息
- **当** 用户 U 不在绑定频道 C 的成员列表中(`getChatMember` 返回 `left`),并在绑定于 C 的评论群 G 中发送一条文本消息 M
- **那么** 系统必须在收到 update 后删除消息 M,且不得向 U 发送任何私聊或群内提示

#### 场景:被踢出频道的用户发消息
- **当** 用户 U 在绑定频道 C 的状态为 `kicked`,并在评论群 G 中发送消息 M
- **那么** 系统必须删除消息 M

### 需求:已关注绑定频道的用户消息必须被放行

对于绑定评论群中由普通用户发送的消息,若 `getChatMember` 返回 `creator`、`administrator`、`member` 或 `restricted`(即用户仍在频道内),系统必须不删除该消息,并将该用户在该群的验证结果写入 `verified_members` 缓存,`expires_at` 设为当前时间 + 配置的 TTL(默认 30 分钟)。`verified_members` 缓存必须以当前绑定的 `channel_chat_id` 为维度之一;对一个 `channel_chat_id` 的放行记录必须不能用于另一个 `channel_chat_id` 的查询。`ChatMemberRestricted` 必须额外检查 `is_member` 字段:`is_member == false` 表示用户已退出/被移出,应视为**不在频道**(`StatusLeft`),不得放行。

#### 场景:关注者首次在评论群发言
- **当** 用户 U 是绑定频道 C 的 `member`,首次在评论群 G 中发言
- **那么** 系统必须不删除该消息,且必须在 `verified_members` 表写入一条 `(G, U, now+30min)` 的记录

#### 场景:缓存命中
- **当** 用户 U 的 `verified_members[G, U]` 存在且 `expires_at > now`
- **那么** 系统必须不调用 `getChatMember`,必须不删除该消息

#### 场景:缓存过期后重新校验
- **当** 用户 U 的 `verified_members[G, U].expires_at <= now`,且此时 U 在评论群 G 发新消息
- **那么** 系统必须重新调用 `getChatMember` 并按当前结果决策;若仍是成员,必须更新缓存 `expires_at`

#### 场景:受限但已离开频道的用户发消息
- **当** 绑定频道 C 对用户 U 返回 `ChatMemberRestricted` 且 `is_member == false`
- **那么** 系统必须视 U 为非成员,必须删除其在评论群的消息

### 需求:绑定频道自身的根帖必须永不被删除

评论群中 `message.sender_chat.id` 等于当前绑定 `channel_chat_id` 的消息(由 Telegram 在频道发帖时自动转发生成的讨论区根帖),系统必须立即放行,不得调用 `getChatMember` 或 `deleteMessage`。特殊情形:若该根帖的正文恰好是一条纯命令(如 `/bind`),则纯命令短路优先于本分支判定,返回 `DecisionIgnore` / `ReasonCommand`;结果仍为"不删除",仅日志中的 reason 不同。

#### 场景:频道发帖触发的讨论区根帖
- **当** 绑定频道 C 发布一条帖子,Telegram 在评论群 G 自动生成 `sender_chat.id == C` 的根消息
- **那么** 系统必须放行此消息,不得删除

#### 场景:频道根帖的正文恰为纯命令
- **当** 绑定频道 C 发布内容恰为 `/bind` 的帖子,Telegram 在评论群 G 生成 `sender_chat.id == C`、`text == /bind` 的根消息
- **那么** 系统必须保留此消息,返回 `DecisionIgnore` / `ReasonCommand`(短路由命令路径命中),不得路由到命令 handler,不得删除

### 需求:以其他频道身份发言的消息必须被删除

评论群中 `message.sender_chat` 非空且**不等于**当前绑定 `channel_chat_id` 的消息(用户或他人以别的频道身份发言),系统必须直接删除,禁止走普通用户的成员校验路径。除非该消息为绑定群的匿名管理员发言(`sender_chat.id == message.chat.id` 且 `from.id == 1087968824`)且配置允许匿名管理员时,此时必须放行。

#### 场景:用户以无关频道身份发广告
- **当** 评论群 G 收到 `sender_chat.id = X` 的消息,且 `X != G 的 bindings.channel_chat_id`
- **那么** 系统必须删除该消息

#### 场景:以不相关频道身份发言但 from.id 是匿名管理员系统 bot
- **当** 评论群 G 收到 `sender_chat.id = X`(`X != G 且 X != 绑定频道`)、`from.id == 1087968824` 的消息,即使配置 `allow_anonymous_admin == true`
- **那么** 系统必须删除该消息,理由为 `other_sender_chat`

### 需求:被编辑的消息必须重新校验

对于 `edited_message` 类型的 update,系统必须按与新消息完全相同的决策流水线重新判定;若决策为删除,必须调用 `deleteMessage` 删除编辑后的消息。

#### 场景:用户先通过后编辑为广告
- **当** 用户 U 通过校验后发送普通消息 M,随后将 M 编辑为包含推广链接的内容,触发 `edited_message` update
- **那么** 系统必须按当前 U 的成员状态重新校验;若 U 仍在频道内,放行;若 U 已退出频道且缓存已过期,必须删除编辑后的 M

### 需求:白名单身份必须短路放行

系统必须在调用 `getChatMember` 之前识别以下发送者并直接放行,以降低 API 调用:bot 自身、配置项 `bot_allowlist` 中列出的 bot、`message.from.id == 1087968824`(Telegram `GroupAnonymousBot`,代表群匿名管理员,当 `allow_anonymous_admin == true` 时放行)。

#### 场景:匿名管理员发消息且配置允许
- **当** 评论群 G 收到 `from.id == 1087968824` 的消息,且配置 `allow_anonymous_admin == true`
- **那么** 系统必须放行,不调用 `getChatMember`

#### 场景:匿名管理员以群身份发言
- **当** 评论群 G 收到 `sender_chat.id == G 本身` 与 `from.id == 1087968824` 的消息,配置 `allow_anonymous_admin == true`
- **那么** 系统必须放行,必须不调用 `getChatMember`,必须不删除

#### 场景:白名单 bot 发消息
- **当** 某 bot B 的 user_id 在 `bot_allowlist` 中,并在评论群 G 发消息
- **那么** 系统必须放行

### 需求:getChatMember 调用失败时必须默认放行并记录告警

当 `getChatMember` 返回错误(网络错误、Telegram 非 429 错误、429 限速),系统必须放行当前消息,不得删除;必须记录 WARN 级别结构化日志,包含 `group_id`、`user_id`、`error`、`retry_after`(若有)。

#### 场景:Telegram API 返回 429
- **当** 系统调用 `getChatMember(C, U)` 收到 HTTP 429 响应
- **那么** 系统必须放行当前消息,必须记录 WARN 日志含 `retry_after`,不得删除任何消息

#### 场景:网络超时
- **当** `getChatMember` 在 5 秒内未返回响应,context 超时
- **那么** 系统必须放行当前消息,必须记录 WARN 日志

### 需求:同一 media_group 必须只校验一次

对于同一 `media_group_id` 的连续消息,系统必须对媒体组内**第一条**消息执行完整决策,并将决策结果在内存中缓存至少 60 秒,供后续同组消息复用;不得对每条消息独立调用 `getChatMember`。此 dedup 仅适用于**新消息**(`message` update);对 `edited_message` 更新必须不查也不写入 dedup,以确保编辑时重新走完整校验流水线。dedup 必须具备 single-flight 语义:多个 goroutine 并发处理同一 `media_group_id` 时,第一个到达的 goroutine 计算决策并发布,其余 goroutine 必须阻塞等待该决策后复用,而不得各自独立调用 `getChatMember`。

#### 场景:用户发送 4 张图的相册
- **当** 非成员 U 发送 `media_group_id = MG` 的 4 条消息
- **那么** 系统必须对第一条调用 `getChatMember` 得出"删"结论,后续 3 条必须直接复用该结论删除,`getChatMember` 累计被调用次数必须为 1

#### 场景:首条决策来自 getChatMember 错误时同组仍必须复用
- **当** 非成员 U 发送 `media_group_id = MG` 的 4 条消息,首条的 `getChatMember` 调用失败
- **那么** 系统必须对首条默认放行(`ReasonErrorDefaultAllow`),并将结论写入 media group dedup;后续 3 条必须直接复用该结论(`ReasonMediaGroupDedup`),`getChatMember` 累计调用次数必须为 1

#### 场景:相册内编辑消息必须重新校验
- **当** 相册内首条消息在 60s dedup 窗口内被编辑
- **那么** 系统必须不查 media_group dedup,必须按编辑后的内容重新调用 `getChatMember` 或使用最新缓存判定;原 dedup 条目必须不变

#### 场景:并发处理同相册消息时只调一次 getChatMember
- **当** bot 进程并发收到同一 `media_group_id` 的 4 条消息(由 runtime 的独立 goroutine 处理),且初始 dedup 缓存为空
- **那么** 首个 goroutine 必须作为 leader 调用 `getChatMember` 并发布决策,其余 3 个 goroutine 必须阻塞等待该 leader 的结果后复用;`getChatMember` 累计被调用次数必须恰好为 1

### 需求:服务消息与频道帖子更新必须被忽略

`message.new_chat_members`、`message.left_chat_member`、`message.pinned_message`、`message.new_chat_title`、`video_chat_started`、`video_chat_ended` 等 Telegram 服务消息,以及 `channel_post` 和 `edited_channel_post` 类型的 update,系统必须直接忽略,不走删除流水线。

#### 场景:有人被加入群
- **当** 评论群 G 收到 `new_chat_members` 服务消息
- **那么** 系统必须不调用 `getChatMember`,必须不调用 `deleteMessage`

#### 场景:群内视频聊天开始
- **当** 评论群 G 收到 `video_chat_started` 服务消息
- **那么** 系统必须不调用 `getChatMember`,必须不调用 `deleteMessage`

### 需求:只有"纯命令"消息才可短路 gating

当消息 `text` 是一条已注册的 bot 命令的纯调用(`/command` 或 `/command@bot_username`,其后无任何实际文本)时,系统必须在 gating 流水线中返回 `Ignore`(reason: `command`),不得调用 getChatMember、不得删除。当消息 `text` 以 `/command` 起始但后接任意用户文本(如 `/status 买广告`),必须视为普通用户消息走完整 gating 流水线;发送者若非频道成员,消息必须被删除。Caption 中的命令字样不得触发短路,媒体消息的 caption 永远不视为命令。该短路仅适用于新消息(`message` update)。对 `edited_message` 更新,即便编辑后文本恰好是一条已注册的纯命令,系统也必须走完整 gating 流水线,不得因命令外形而放行。该短路必须同时覆盖:(1) 本 bot 已注册的纯命令(`/cmd` 或 `/cmd@本 bot username`);(2) 指向其他 bot 的纯命令(`/cmd@其他 bot username`),即便本 bot 未注册该命令也不得删除,以便与其他 bot 共存。此短路在 `sender_chat` 分支之前判定:因此 `/cmd@other_bot`(即便以频道身份发送)和文本恰为 `/cmd` 的频道根帖都视为命令而保留,不会落入 "other sender_chat 必须删除" 分支;此时日志 reason 为 `command` 而非 `channel_root_post`,但结果都是"不删除"。`commandBearingText` 必须与 dispatcher 的 `parse` 保持一致:文本首字符必须是 `/`,有任何前导空白(空格/制表/换行)都不得视为命令。否则 gate 与 dispatcher 会对同一消息给出不一致的判定。

#### 场景:非成员以带尾随文本的命令躲避删除
- **当** 非成员 U 在绑定群 G 发送 `/status buy now`(`/status` 已注册为命令)
- **那么** 系统必须对这条消息走完整 gating,必须调用 `getChatMember` 或使用缓存判定为非成员,必须删除消息,必须不路由到命令 handler

#### 场景:媒体 caption 为命令关键字
- **当** 非成员 U 在绑定群 G 发送一张图片,caption 为 `/status`
- **那么** 系统必须对该图片走完整 gating,必须删除;系统必须不将 caption 识别为命令

#### 场景:将消息编辑为纯命令不得绕过守卫
- **当** 非成员 U 将先前一条普通消息编辑为文本 `/status`(或其他已注册的纯命令)
- **那么** 系统必须对该 edited_message 走完整 gating,调用 `getChatMember` 或读缓存判定非成员,必须删除;必须不路由到命令 handler,也不按命令短路跳过

#### 场景:指向其他 bot 的命令不得被删除
- **当** 非成员 U 在绑定群 G 发送 `/status@other_bot`
- **那么** 系统必须不调用 `getChatMember`,必须不删除;此消息必须留给 other_bot 自行处理

#### 场景:以其他频道身份发送的纯命令不得被删除
- **当** 评论群 G 收到 `sender_chat.id = X`(非绑定频道)、`text = /cmd@other_bot` 的消息
- **那么** 系统必须视为纯命令短路,返回 `DecisionIgnore` / `ReasonCommand`,不得删除

#### 场景:前导空白的命令外形文本不得被命令短路放行
- **当** 非成员 U 发送 " /status"(前导空格)或 "\n/bind"(前导换行)的文本
- **那么** 系统必须不把它视为命令,必须走完整 gating 流水线;若 U 非频道成员,消息必须被删除
