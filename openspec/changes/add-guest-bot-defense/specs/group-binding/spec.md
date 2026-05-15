## 新增需求

### 需求:群级 bot 白名单必须持久化

系统必须提供 `group_bot_allowlist` 表,以 `(group_chat_id, bot_user_id)` 为主键,字段含 `bot_username`、`added_by`、`added_at`。该表必须在进程启动时自动创建(沿用现有 schema 初始化方式)。对某 `group_chat_id` 的群级白名单查询必须只返回该 `group_chat_id` 的条目;一个群的白名单条目不得用于另一个群的判定。bot 改名后其 `user_id` 不变,匹配必须以 `bot_user_id` 为准,`bot_username` 仅用于展示。

#### 场景:启动时自动建表
- **当** bot 进程启动且 `group_bot_allowlist` 表尚不存在
- **那么** 系统必须创建该表后再开始处理 update

#### 场景:群级白名单按群隔离查询
- **当** 查询群 G1 的群级白名单
- **那么** 结果必须只含 `group_chat_id == G1` 的条目,不得混入其他群的条目

#### 场景:重启后白名单恢复
- **当** bot 进程重启
- **那么** 此前写入 `group_bot_allowlist` 的条目必须仍然有效

### 需求:guest-bot 召唤违规计数必须持久化

系统必须以 `(group_chat_id, user_id)` 为键持久化记录每个用户召唤未授权 guest bot 的违规次数,供 channel-gating 的升级惩罚判定使用。该存储必须在进程启动时自动创建,并在进程重启后保留计数。

#### 场景:违规计数重启后保留
- **当** 用户 U 在群 G 已有若干次召唤违规记录,bot 进程随后重启
- **那么** 重启后系统读取到的 U 在 G 的违规次数必须与重启前一致

## 修改需求

### 需求:/unbind 必须删除绑定记录且权限校验与 /bind 相同

`/unbind` 命令必须只能在群内执行,调用者必须是该群的创建者(`MemberStatus == creator`),校验通过后系统必须从 `bindings` 表删除对应 `group_chat_id` 的记录;对应 `verified_members` 中该 `group_chat_id` 的缓存条目也必须级联删除;**对应 `group_bot_allowlist` 中该 `group_chat_id` 的群级白名单条目、以及该群的 guest-bot 召唤违规计数也必须级联删除。** 匿名管理员(`from.id == 1087968824`)自动被此规则拒绝,不需要额外代码路径。

#### 场景:创建者解除绑定
- **当** 群 G 创建者 A 执行 `/unbind`,且 G 已存在绑定
- **那么** 系统必须删除 `bindings[G]`,必须清除 `verified_members` 中所有 `group_chat_id == G` 的记录,必须清除 `group_bot_allowlist` 中所有 `group_chat_id == G` 的条目与该群的召唤违规计数,并回复"已解除绑定"

#### 场景:未绑定的群尝试解绑
- **当** 群 G 不存在绑定记录,创建者 A 执行 `/unbind`
- **那么** 系统必须返回"当前群未绑定任何频道",必须不删除 verified_members 中任何行,必须不修改任何表。系统层面,`DeleteBinding` 仅在实际删除 bindings 行时才级联清理 verified_members、`group_bot_allowlist` 与违规计数。

#### 场景:/unbind 带有尾随文本不被视为命令
- **当** 用户发送 `/unbind anything` 或任何非空尾随文本
- **那么** 系统必须不调用 `/unbind` handler,必须不删除 bindings 表记录;此消息必须按普通消息由 gating 流水线处理,若发送者非频道成员,消息必须被删除
