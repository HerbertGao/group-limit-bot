# Guest Bot 广告：调查结论

> 调查日期：2026-05-15　|　对应 Telegram Bot API 10.0（2026-05-08）

## 结论（TL;DR）

Telegram Bot API 10.0 的 **Guest Mode（访客模式）** 让广告机器人无需加入群组、无需任何权限，就能被人工 `@` 召唤后往群里投放广告。

**这种 guest bot 广告是可以拦截的** —— 前提是守卫 bot 在 `@BotFather` 开启 **Bot-to-Bot Communication Mode**。

开启后：guest 广告会以**普通 `message` update** 进入守卫 bot 的 update 流，`from` 即广告 bot，并带 `guest_bot_caller_user` 字段；`deleteMessage` 可正常删除（全群消失）。本项目现有的 gating 逻辑已能删它，建议再增加基于 `guest_bot_caller_user` 的确定性判据。

> ⚠️ 若**不**开启 Bot-to-Bot Communication Mode，guest 回复对守卫 bot 完全不可见、不可删 —— 这是本次调查早期一度得出"无解"结论的原因。该模式是关键开关。

## Guest Mode 是什么

引用 Telegram 官方对 Guest Mode 与 Inline Mode 的区分：

- **Inline Mode**：用户在输入框打 `@bot` → 选一个结果 → 以**用户自己**的名义发出（消息带 `via_bot`）。
- **Guest Mode**：用户把 `@bot` 作为**一条消息发出**来召唤 → bot **以自己的名义独立回复**。Telegram 的定位是「把 bot 的功能短暂带入聊天，不污染成员列表、不暴露消息、不授予任何权限」。

广告党的手法 —— 把 `@adbot1 @adbot2 @adbot3` 当作一条消息发出 —— 正是 Guest Mode 的召唤触发。一条消息最多可召唤 3 个 guest bot。

```
群成员发: "@adbot 关键词"            ← 召唤消息(summon)
        │
        ▼
@adbot 收到 Update.guest_message      ← 它根本不在群里
        │  调 answerGuestQuery(result=...)
        ▼
群里出现一条来自 @adbot 的广告         ← 广告本体
```

## 关键机制：Bot-to-Bot Communication Mode

引用 Telegram 官方说明：

> Bots with **Bot-to-Bot Communication Mode** enabled will receive **all messages from other bots in groups** without explicit mentions or replies if they:
> - Have admin rights in the group, **or**
> - Have Group Privacy Mode disabled

守卫 bot 本就是群管理员、且 Privacy Mode 关闭 —— 两个条件都满足。一旦开启 Bot-to-Bot Communication Mode，它就会收到群内**所有其他 bot 的消息**，guest 广告回复正是"另一个 bot 发出的消息"，因此变得可见、可删。

> Chat Access Mode（针对 business account）和 Bot Management Mode 与此**无关**，实测已排除。

## 调查过程与实测数据

用两个一次性测试 bot 全程 curl 打 Bot API 验证（不依赖本项目代码）：

- **Bot O** = 观察者，模拟本守卫 bot：群管理员、Privacy Mode 关闭。
- **Bot G** = guest bot：BotFather 中开启 Guest Mode。

### 模式隔离测试

在测试群召唤 Bot G、用 `answerGuestQuery` 应答，观察 Bot O 的 `getUpdates`：

| Bot O 模式组合 | 是否收到 guest 回复 |
|---|---|
| 三模式全开 | ✅ 收到 |
| 仅 Chat Access Mode | ❌ 收不到 |
| Chat Access + **Bot-to-Bot Communication** | ✅ 收到 |

结合官方文档 → **Bot-to-Bot Communication Mode 是唯一关键开关**。

### guest 回复消息的结构（开启后 Bot O 收到的）

```
message:
  message_id:  19            ← 普通 message_id,配合 chat_id 即可 deleteMessage
  chat.id:     <群 id>
  from:        { id:<广告bot>, is_bot:true, username:"..." }   ← 发送者就是广告 bot
  reply_to_message:           ← 指向召唤消息
  text / entities:            ← 广告正文
  guest_bot_caller_user:      ← 召唤者(发起 @ 的那个人) —— 确定性判据
```

### 删除验证

`deleteMessage(chat_id, message_id)` 由守卫 bot（群管理员）调用 → 返回 `{"ok":true,"result":true}`，广告**全群消失**。

### 早期"无解"阶段的观察（未开启 Bot-to-Bot 时）

- guest 回复是 inline message（`answerGuestQuery` 返回 `SentGuestMessage{inline_message_id}`），不产生任何第三方可见的 update。
- 真实广告 `tdxzmnrbot`（非群成员）的广告，转发副本与"已知 guest bot"的回复转发副本**逐字段结构一致** → 实锤真实广告走的就是 Guest Mode。
- 渲染差异（老客户端显示折叠/`via`、新版正常显示"for 召唤者"）纯属客户端版本问题，与拦截无关。

### 没有原生群级开关

群「设置 → 权限」无任何 guest 相关开关；`restrictChatMember` 管不到 guest bot（它不是成员）。拦截只能靠守卫 bot 收到消息后删除。

## 落地方案

### 能力天花板：无法预防"第一条"广告

召唤消息一旦发出，Telegram 服务端**即时**触发 guest bot，guest bot（预制广告内容）毫秒级应答。守卫 bot 只能通过 long-poll 在消息**发出之后**才看到它 —— 删召唤、罚召唤者都拦不住**这一条**广告。Telegram 自身的反垃圾（Aggressive mode）同样是事后删，这是 Bot API 的能力上限。

因此目标定为两个可达成项：**① 把广告闪现时间压到最短；② 让"再发一条"变得不可能**。

```
召唤消息发出 ──同一瞬间──▶ 服务端触发 guest bot ──▶ 广告出现
   (人的动作)              (服务端,即时,碰不到)        │
        │                                              │
   我们 long-poll 收到 summon ─────────── 我们永远在"事后" ┘
```

### 部署前提（必备）

在 `@BotFather` 为守卫 bot 开启 **Bot-to-Bot Communication Mode**。与「关闭 Privacy Mode」并列，写入 README 部署前置条件。不开则 guest 回复完全不可见。

### L1 — 最快移除（压缩闪现时间）

收到 guest 回复那条 update 时，它自带删除所需的一切：

- `chat.id` + `message_id` → 删广告本体；
- `reply_to_message.message_id` → **即召唤消息的 id**，同一条 update 即可顺手删掉召唤，无需自行存储召唤消息。

延迟 ≈ long-poll 投递 + 一次 `deleteMessage` RTT，通常 1 秒内。附带好处：广告删得快 → 没有"旧广告消息"可供他人**回复来二次召唤**（guest bot 也能被"回复其消息"触发），快删顺带堵掉再召唤入口。

### L2 — 惩罚召唤者（针对"重复"的真正预防）

`guest_bot_caller_user` 直接给出召唤者身份。判定**不看关注状态**，只看「召唤的是不是一个未被白名单放行的 guest bot」—— 关注门禁与 guest-bot 防御是两件正交的事，关注了频道不豁免"召唤广告 bot"这一独立违规。

- 第 1 次：删广告 + 删召唤（可选警告）；
- 重复：**禁言**（`restrictChatMember`，`can_send_messages=false`），再升级可 ban。

禁言是此处唯一真正"预防性"的动作 —— 被禁言者发不出召唤消息，其后续广告被从源头掐断。效果：无论召唤者是路人还是已关注用户，重复作恶都变成"每条广告烧一个能发言的账号"。

### L3 — 新成员禁言窗口（可选，部分预防）

入群时 `restrictChatMember` 给新成员一段禁言期、到点自动解。批量新注册小号在窗口期内发不出召唤 → 对"新号刷广告"是真预防。代价：影响正常新人、拦不住养号与已关注的老号。属可选加强层。

### L4 — 默认禁言模型（架构级，不推荐）

把"懒校验删除"改为"默认全员禁言、验证为关注者才解禁"，使非关注者物理上无法发召唤。但与项目刻意选择的 lazy-delete 设计冲突、UX 伤害大，且对"已关注用户召唤广告"完全无效。仅为完整性列出。

### 实现要点

- **确定性判据**：检测消息上的 `guest_bot_caller_user` / `guest_bot_caller_chat` / `guest_query_id` 字段 → 直接判定为 guest 回复。零 API 成本、不依赖 `getChatMember`、规避其对 bot id 查询报错时 fail-open 的风险。该检查应放在 `getChatMember` 路径**之前**。（现有 gating 逻辑虽也能删 —— bot、非频道成员、非白名单 → 删 —— 但确定性字段更稳。）
- **白名单豁免**：若 guest bot 的 id 在 `allowlist` 中（运营者主动想用某个 guest AI 工具），既不删也不罚。复用现有配置项。
- **依赖**：`telego v1.8.0` 源码无 `guest_*` 字段（早于 Bot API 10.0）→ 需升级 telego，或对这几个字段做 raw-JSON 解码。
- **update 量**：开启 Bot-to-Bot Communication Mode 后会收到群内所有 bot 的消息，量上升 —— 可接受，需留意。
- **Loop Prevention**：官方对 bot-to-bot 有死循环警告。本守卫 bot 只删除、不回复 bot，无互相触发风险；删除仍受 Telegram 限速约束，沿用现有 executor 的限速处理即可。

## 群级 bot 白名单

L2 的「白名单豁免」若只靠进程级全局配置，改一次要重启、且无法按群区分。为此引入**群级** bot 白名单，让每对（频道, 群）的运营者用命令自管「我这个群允许哪个 bot / guest bot」。

- **分层**：生效白名单 = 全局 `allowlist`（进程级，config 注入）**∪** 当前群的群级白名单。某 bot 在任一份里即放行。
- **数据表** `group_bot_allowlist`：

  ```
  group_chat_id  INTEGER  ┐
  bot_user_id    INTEGER  ┴ PRIMARY KEY(group_chat_id, bot_user_id)
  bot_username   TEXT     ← 仅供展示,可能过期;bot_user_id 才是权威键
  added_by       INTEGER  ← 添加它的群创建者 user_id
  added_at       INTEGER
  ```

- **命令**（仅群创建者，与 `/bind` 一致）：
  - `/allowbot @somebot` —— `getChat` 解析 `@username` 为数字 id 入库；重复添加幂等（提示「已在白名单」）。可选支持「回复某 bot 消息发 `/allowbot`」，从 `reply_to_message.from` 直取 id。
  - `/disallowbot @somebot` —— 从本群白名单移除。
  - 列表并入 `/status` 输出，不单设列表命令。
- **gate 改动**：`decideBase` 中 bot 白名单短路由 `BotAllowlist[from.ID]` 改为 `全局[from.ID] || 群级(binding.GroupChatID)[from.ID]`。群级集合按群懒加载进内存缓存，命令增删时失效。
- **生命周期**：`/unbind` 解绑时一并清空该群的 `group_bot_allowlist` 条目与 guest 召唤违规计数（与解绑清缓存一致）。
- bot 改名不影响匹配（存的是 id）；误加一个非 bot 的 id 是无害死数据（gate 仅在 `from.IsBot` 时查白名单）。

## 残留风险 / 未验证项

- `getChatMember` 对 guest bot 的 user id 查询的具体行为未单独验证（正常返回 `left` 还是报错）—— 这正是推荐用 `guest_bot_caller_user` 确定性字段、而非依赖 `getChatMember` 的原因。
- 本次在基础 `group` 验证；真实频道评论区是 `supergroup`。机制相同，预期一致，但建议上线前在 supergroup 复验一次。

## 参考

- Telegram 博客：<https://telegram.org/blog/ai-bot-revolution-11-new-features>
- Bot API changelog（10.0）：<https://core.telegram.org/bots/api-changelog>
- Bot API 文档：<https://core.telegram.org/bots/api>
- Bot-to-Bot Communication 官方说明（@BotFather 文档 / Bot Features）
