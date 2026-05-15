## 新增需求

### 需求:/allowbot 与 /disallowbot 命令必须仅对群创建者可用,用于维护群级 bot 白名单

`/allowbot` 与 `/disallowbot` 命令必须只能在已绑定的评论群内执行;命令执行前必须调用 `getChatMember(group_id, caller_user_id)` 并要求 `creator`(严格相等),非创建者必须收到拒绝回复且不得修改任何表。`/allowbot` 的参数必须为 bot 用户名;系统必须接受带或不带前导 `@` 的输入并归一化为 `@username` 后交 `getChat` 解析为数字 `user_id`。解析得到的 id 必须为正整数;若解析得到非正 id(频道/群)或 `getChat` 解析失败,系统必须回复错误说明,禁止写入 `group_bot_allowlist`。解析成功时系统必须向 `group_bot_allowlist` 表写入 `(group_chat_id, bot_user_id, bot_username, added_by, added_at)`;若该 bot 已存在于本群白名单,系统必须以幂等方式回复"该 bot 已在白名单",禁止报错。`/disallowbot @username` 必须从 `group_bot_allowlist` 删除当前群对应该 bot 的条目。命令缺少参数时,系统必须回复错误说明,禁止写入残缺记录。

#### 场景:创建者添加 bot 到群级白名单
- **当** 群 G 创建者 A 执行 `/allowbot @somebot`,`@somebot` 可被 `getChat` 解析
- **那么** 系统必须在 `group_bot_allowlist` 写入一条 `(G, somebot 的 user_id, somebot, A, now)` 记录,并回复成功

#### 场景:重复添加幂等
- **当** 创建者 A 对一个已在群 G 白名单中的 bot 再次执行 `/allowbot`
- **那么** 系统必须回复"该 bot 已在白名单",禁止报错,禁止产生重复行

#### 场景:创建者移除 bot
- **当** 群 G 创建者 A 执行 `/disallowbot @somebot`,该 bot 在 G 的白名单中
- **那么** 系统必须从 `group_bot_allowlist` 删除该条目并回复成功

#### 场景:非创建者执行命令
- **当** 非创建者在群 G 执行 `/allowbot` 或 `/disallowbot`
- **那么** 系统必须回复权限不足提示,禁止修改 `group_bot_allowlist`

#### 场景:用户名无法解析
- **当** 创建者执行 `/allowbot @notexist`,`getChat` 对该用户名返回错误
- **那么** 系统必须回复错误说明,禁止向 `group_bot_allowlist` 写入任何记录

#### 场景:参数不带 @ 前缀
- **当** 创建者执行 `/allowbot somebot`(参数不带 `@`)
- **那么** 系统必须归一化为 `@somebot` 再经 `getChat` 解析,解析成功则正常写入

#### 场景:用户名解析到非用户实体
- **当** 创建者执行 `/allowbot @somechannel`,`getChat` 解析得到非正 id(频道/群)
- **那么** 系统必须回复错误说明,禁止向 `group_bot_allowlist` 写入任何记录

## 修改需求

### 需求:/status 命令必须仅对群创建者可用,且输出绑定与运行状态

`/status` 命令必须在执行前调用 `getChatMember(group_id, caller_user_id)`,要求 `creator`(严格相等);非创建者必须收到拒绝回复;权限通过后系统必须在群内回复一条消息,包含:当前 `group_chat_id`、绑定的 `channel_chat_id` 与频道标题、`verified_members` 中该群的有效条目数、近 1 小时该群的删除消息计数、最近 5 次 `getChatMember` 错误的时间戳与错误信息、**以及当前群的群级 bot 白名单(`group_bot_allowlist` 中该 `group_chat_id` 各条目的 `bot_username` 与 `bot_user_id`)**。"已验证成员"计数必须仅统计当前生效绑定的 `channel_chat_id` 下的行,不得把历史其他频道残留的 `verified_members` 行计入。若 `CountVerifiedInChannel` 返回错误(例如 SQLite 锁冲突),系统必须回复"内部错误,读取状态失败"而非用 0 作为伪造数字继续渲染;原命令消息与错误回复按与其他拒绝路径一致的方式在 10 秒后被清理。当读取绑定频道元数据 (`GetChat(channel)`) 失败时,系统必须回复"内部错误,读取频道信息失败"而非用 "(未知)" 占位符继续渲染正常报告。状态报告回复失败(SendMessage 返错)时必须记 ERROR 日志并向 runtime 返回错误;不得伪装成功。与 /bind 成功后回复失败的行为保持一致。

#### 场景:创建者查询状态
- **当** 创建者 A 在群 G 内执行 `/status`,G 已绑定到频道 C
- **那么** 系统必须回复一条文本消息,包含字段:`group`、`channel`、`cached_members`、`deletes_1h`、`recent_errors`、`group_bot_allowlist`

#### 场景:非创建者查询状态
- **当** 非创建者(普通成员或管理员)在群 G 内执行 `/status`
- **那么** 系统必须回复权限不足提示,必须不泄露任何运行时状态字段

#### 场景:未绑定的群执行 /status
- **当** 创建者 A 在未绑定的群 G 执行 `/status`
- **那么** 系统必须回复"当前群未绑定任何频道",必须不查询 `verified_members` 或删除计数

#### 场景:/status 读取验证缓存计数失败
- **当** 创建者执行 `/status`,后端 `CountVerifiedInChannel` 返回错误
- **那么** 系统必须回复"内部错误,读取状态失败",不得输出带有 0 条验证成员的伪造状态块,必须清理原命令与错误回复

#### 场景:/status 读取频道元数据失败
- **当** 创建者执行 `/status`,绑定存在,但 `GetChat(channel)` 返回错误
- **那么** 系统必须回复"内部错误,读取频道信息失败",必须不输出带占位符的状态块,必须清理原命令与错误回复

#### 场景:群级白名单输出
- **当** 创建者 A 在已绑定且 `group_bot_allowlist` 含若干条目的群 G 执行 `/status`
- **那么** 系统回复必须列出该群每个白名单 bot 的 `bot_username` 与 `bot_user_id`
