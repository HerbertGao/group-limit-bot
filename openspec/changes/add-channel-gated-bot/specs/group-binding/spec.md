## 新增需求

### 需求:/bind 必须在目标评论群内执行且不接受参数

`/bind` 命令必须只能在群组类型(`supergroup` 或 `group`)的聊天中执行;命令必须不接受任何文本参数——bot 必须从 `getChat(chat_id).linked_chat_id` 自动推导绑定的频道 id。若在非群组聊天(如私聊或频道)执行,必须返回说明性错误信息,不得创建绑定记录。绑定持久化成功后,如果 `SendMessage` 回复失败(例如 MarkdownV2 解析错、API 超时),handler 必须记 ERROR 日志并返回错误,不得伪装成功。原命令消息与任何成功到达的 reply 仍按自动清理策略处理。

#### 场景:创建者在评论群内执行 /bind
- **当** 群 G 的创建者 A 在群内发送 `/bind`
- **那么** 系统必须调用 `getChat(G)` 获取 `linked_chat_id`,并以该值作为 `channel_chat_id` 完成绑定,不得要求 A 填写参数

#### 场景:在私聊中执行 /bind
- **当** 用户 A 在与 bot 的私聊中发送 `/bind`
- **那么** 系统必须回复错误消息说明该命令仅限群组使用,必须不创建任何绑定

#### 场景:/bind 带有尾随文本不被视为命令
- **当** 用户发送 `/bind foo` 或任何非空尾随文本
- **那么** 系统必须不调用 `/bind` handler,必须不调用 `getChat` 与 `getChatMember`,必须不创建或更新绑定;此消息必须按普通消息由 gating 流水线处理,若发送者非频道成员,消息必须被删除

### 需求:/bind 必须校验调用者是目标群的创建者

执行 `/bind` 时,系统必须调用 `getChatMember(group_id, caller_user_id)`;返回状态必须为 `creator`(`MemberStatus == creator`)。否则系统必须拒绝本次绑定并回复"仅群创建者可执行本命令"。匿名管理员(`from.id == 1087968824`)自动被此规则拒绝,不需要额外代码路径。

#### 场景:非创建者尝试绑定
- **当** 非创建者 U(例如普通成员或管理员)在群 G 内发送 `/bind`
- **那么** 系统必须返回错误信息,不得写入 `bindings` 表

### 需求:/bind 必须校验 bot 是目标频道的管理员

执行 `/bind` 前,系统必须调用 `getChatMember(channel_id, bot_id)` 确认 bot 在 `linked_chat_id` 对应的频道中是 `administrator`;否则必须拒绝绑定并提示管理员将 bot 添加为频道管理员。当 `getChatMember(channel, bot)` 返回非瞬态 API 错误(如 400/403,或任何非 `*RateLimitError` / `context.DeadlineExceeded` / `context.Canceled` 的错误),必须同样视为 bot 非频道管理员并返回相同的指引错误,避免首次部署场景被伪装成 "内部错误"。

#### 场景:bot 未被加为频道管理员
- **当** 创建者 A 在群 G 执行 `/bind`,但 bot 在 G 的 linked 频道 C 中不是管理员
- **那么** 系统必须返回错误信息"请先将 bot 加为频道 C 的管理员",不得写入 `bindings` 表

### 需求:/bind 必须校验 bot 在评论群中有删除消息权限

执行 `/bind` 前,系统必须调用 `getChatMember(group_id, bot_id)` 并检查返回:
- `creator` 身份隐含所有权限,视为通过;
- `administrator` 身份必须且 `can_delete_messages == true`,否则拒绝绑定并提示管理员为 bot 授予"删除消息"权限;
- 其他任何状态必须拒绝绑定。

当 `getChatMember(group, bot)` 返回非瞬态 API 错误(如 400/403,或任何非 `*RateLimitError` / `context.DeadlineExceeded` / `context.Canceled` 的错误),必须同样视为 bot 在群中无删除权限并返回相同的指引错误,避免首次部署场景被伪装成 "内部错误"。

#### 场景:bot 在群内不是管理员
- **当** bot 在讨论群 G 内状态为 `member` 或未加入,创建者 A 执行 `/bind`
- **那么** 系统必须返回"bot 在本群没有删除消息权限..."的说明性错误,不得写入 `bindings` 表

#### 场景:bot 是群管理员但缺少删除消息权限
- **当** bot 在讨论群 G 内状态为 `administrator` 但 `can_delete_messages == false`
- **那么** 系统必须返回错误,不得写入 `bindings` 表

### 需求:/bind 必须拒绝未关联 linked_chat_id 的群

若 `getChat(group_id).linked_chat_id` 为空(该群未在 Telegram 侧关联任何 discussion channel),系统必须拒绝绑定并返回说明性错误信息指引运营者先到 Telegram 频道设置中完成 discussion group 关联。

#### 场景:群未被设置为讨论群
- **当** 群 G 的 `linked_chat_id` 为空,创建者 A 执行 `/bind`
- **那么** 系统必须返回错误信息,不得写入 `bindings` 表

### 需求:/bind 必须支持覆盖已有绑定

若目标 `group_chat_id` 已存在 `bindings` 表记录,系统必须以 upsert 方式更新该行的 `channel_chat_id`、`bound_by_user_id` 与 `bound_at`,并在返回消息中明确提示"已更新绑定"以区别于首次绑定。当 `channel_chat_id` 发生变化时必须级联清空 verified_members 与内存缓存,防止旧频道的缓存复用到新频道。为防止重绑竞态下旧频道的成员校验结果被写回覆盖后的缓存,`verified_members` 必须以 `(group_chat_id, channel_chat_id, user_id)` 为主键,内存缓存键同样包含 `channel_chat_id`。cache 写入必须与 `bindings` 表存在性原子校验同事务执行;此校验必须通过**单个 SQL 语句**(`INSERT … SELECT … WHERE EXISTS … ON CONFLICT DO UPDATE`)完成,不得使用 SELECT + INSERT 两步事务的组合,因为 SQLite 默认隔离下两步之间可能有其他事务提交 `/unbind`,导致写入孤儿 verification row。当 `(group_chat_id, channel_chat_id)` 绑定不存在时,必须不写入 `verified_members`,防止 `/unbind` 后飞行中的决策把批准重新灌回被复用的旧缓存。此外,`cache.Set` 在写入内存缓存前必须比对 per-group 的 generation 计数器;`DropGroup` 必须递增该计数器。若 generation 在 SQL commit 期间发生变更(说明 `/unbind` 已执行),必须不写入内存,以防止旧近似写回。epoch 必须在 bindings 表外以独立的 `binding_epochs(group_chat_id, last_epoch)` 表维护单调递增计数。`UpsertBinding` 必须在同一事务内递增该计数并作为新的 epoch 写入 bindings,`DeleteBinding` 必须不触碰该计数。这样即便 `/unbind` 后立即重绑同一频道,epoch 也严格大于任何在途 Set 所持有的旧值,防止 stale 批准越过原子校验。此 epoch 必须传递到 `verified_members` 的写入路径,`UpsertVerifiedIfBound` 必须 WHERE `bindings.epoch = ?`,以确保 `/unbind → /bind` 到同一频道的重绑竞态下,旧 epoch 的在途 Set 无法落库。

#### 场景:重复绑定同一群
- **当** 群 G 已有绑定,创建者 A 再次执行 `/bind`
- **那么** 系统必须更新 `bindings` 记录而非插入新行,必须保持 `group_chat_id` 主键唯一,返回消息必须区分"已创建"与"已更新"

#### 场景:覆盖绑定时若频道变更必须清空该群缓存
- **当** 群 G 原本绑定到频道 C1,创建者执行 `/bind`,此时 G 的 `linked_chat_id` 变为 C2
- **那么** 系统必须将 `bindings[G].channel_chat_id` 更新为 C2,并且必须清空 `verified_members` 中所有 `group_chat_id == G` 的行,且必须同步丢弃内存 `MemberCache` 中该群的条目

#### 场景:/unbind 后飞行中的 cache.Set 被阻止
- **当** 创建者执行 `/unbind` 清空 bindings[G],同一时刻仍有飞行中的 gate 决策将要为 U 写入 `(G, C, U)` 的验证缓存
- **那么** 系统必须在写入时检查 bindings 表,若绑定不存在必须不写入 `verified_members`,内存缓存也必须不更新;下次重新 `/bind G→C` 后 U 的消息必须重新调用 `getChatMember` 校验

#### 场景:unbind 后立即重绑同频道,旧 getChatMember 的 Set 不得写入
- **当** gate 在 epoch=N 时读取 binding 并发起 getChatMember,之后在写 verified_members 前,`/unbind` 成功 + `/bind` 重新绑定到同一频道(epoch 变为 N+2)
- **那么** `UpsertVerifiedIfBound(G, C, U, epoch=N, ...)` 必须返回 applied=false,不得在新绑定的 epoch 下留下旧 getChatMember 的批准;该用户下一条消息必须触发新的 `getChatMember` 调用

#### 场景:unbind 后立即重绑同频道,旧 epoch 绝不复用
- **当** gate 在 epoch=N 读到绑定后发起 getChatMember,其间 `/unbind` + `/bind` 同频道成功执行
- **那么** `bindings.epoch` 必须严格大于 N(至少 N+2),`UpsertVerifiedIfBound(..., epoch=N, ...)` 必须返回 applied=false,不得在新绑定下留下旧 getChatMember 的批准

### 需求:/unbind 必须删除绑定记录且权限校验与 /bind 相同

`/unbind` 命令必须只能在群内执行,调用者必须是该群的创建者(`MemberStatus == creator`),校验通过后系统必须从 `bindings` 表删除对应 `group_chat_id` 的记录;对应 `verified_members` 中该 `group_chat_id` 的缓存条目也必须级联删除。匿名管理员(`from.id == 1087968824`)自动被此规则拒绝,不需要额外代码路径。

#### 场景:创建者解除绑定
- **当** 群 G 创建者 A 执行 `/unbind`,且 G 已存在绑定
- **那么** 系统必须删除 `bindings[G]`,必须清除 `verified_members` 中所有 `group_chat_id == G` 的记录,并回复"已解除绑定"

#### 场景:未绑定的群尝试解绑
- **当** 群 G 不存在绑定记录,创建者 A 执行 `/unbind`
- **那么** 系统必须返回"当前群未绑定任何频道",必须不删除 verified_members 中任何行,必须不修改任何表。系统层面,`DeleteBinding` 仅在实际删除 bindings 行时才级联清理 verified_members。

#### 场景:/unbind 带有尾随文本不被视为命令
- **当** 用户发送 `/unbind anything` 或任何非空尾随文本
- **那么** 系统必须不调用 `/unbind` handler,必须不删除 bindings 表记录;此消息必须按普通消息由 gating 流水线处理,若发送者非频道成员,消息必须被删除

### 需求:系统必须支持单 bot 实例承载多对绑定

`bindings` 表必须以 `group_chat_id` 为主键,允许多行记录;不同 `group_chat_id` 可以指向同一 `channel_chat_id`(多个评论群共享同一频道门槛);update 路由必须基于 `chat.id` 从 `bindings` 表中查询当前生效的 `channel_chat_id`,不得依赖全局单绑定变量。

#### 场景:两个群绑定到同一频道
- **当** 群 G1 和 G2 先后绑定到同一频道 C
- **那么** `bindings` 表必须存在两行记录,`G1 → C` 与 `G2 → C`,两群的消息必须各自独立走决策流水线并共用对 C 的成员查询缓存语义

#### 场景:未绑定的群消息被忽略
- **当** 评论群 G 未在 `bindings` 表中,且 G 内有用户发消息
- **那么** 系统必须不调用 `getChatMember`,必须不删除该消息

### 需求:/bind 与 /unbind 的命令消息与回复必须在 10 秒内自动删除

系统必须在处理完 `/bind` 或 `/unbind` 命令(无论成功或失败)后,约 10 秒延迟内删除:
- 触发命令的消息(`msg.MessageID`);
- bot 发出的回复消息。

原命令消息和 bot 的回复必须在约 10 秒延迟后一并删除(而非立即删除),以便操作者在清理前能将回复与自己触发的命令关联起来;拒绝路径也不得提前同步删除命令消息。删除必须为尽力而为,失败时仅记录 debug 日志,不得阻塞后续请求。`/status` 的拒绝性回复同样走自动清理,但成功的状态报告必须保留,详见 admin-commands 规格中"/status 的拒绝性回复必须自动清理,成功状态报告必须保留"。

#### 场景:/bind 成功后自动清理
- **当** 管理员执行 `/bind` 成功
- **那么** 系统必须在约 10 秒后调用 `deleteMessage` 删除原命令消息与 bot 的成功回复

#### 场景:/bind 因权限不足被拒绝后自动清理
- **当** 非创建者触发 `/bind`,被系统拒绝并回复错误
- **那么** 系统必须在约 10 秒后删除原命令消息与错误回复

#### 场景:/unbind 处理完成后自动清理
- **当** 创建者触发 `/unbind`,无论成功或失败
- **那么** 系统必须在约 10 秒后删除原命令消息与 bot 的回复

### 需求:绑定关系必须持久化且重启后恢复

`bindings` 表必须存储在 SQLite 持久文件中(路径由配置项 `db_path` 指定);进程重启后必须自动打开同一数据库并从 `bindings` 表加载全部绑定;系统不得依赖内存单一来源的绑定状态。

#### 场景:进程重启
- **当** 已存在绑定 `G → C` 且 bot 进程重启
- **那么** 重启后第一条来自 G 的消息必须仍然按 `G → C` 进行决策,不要求管理员重新执行 `/bind`
