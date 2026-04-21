# group-limit-bot

Telegram 守卫 bot:保护**频道 linked 评论群**,仅放行已关注绑定频道的用户。未关注者的消息会被**静默删除**,建议在频道与群公告中向用户说明该政策。

单 bot 实例可同时承担多对(频道, 评论群)。

## 部署最小步骤

### 1. 创建 bot

到 `@BotFather` 执行:

1. `/newbot` → 拿到 `bot_token`
2. `/setprivacy` → 选择你的 bot → **Disable**(关键!关闭后 bot 才能收到群内普通消息,而不是只能收到命令)
3. `/setjoingroups` → 选择 `Enable`(允许 bot 被加入群组)

### 2. 把 bot 加进频道和群

- **频道**:以**管理员**身份加入(需要权限: 查看成员 + 删除消息即可,其他不需要)
- **评论群**(频道 linked 的 discussion group):以**管理员**身份加入(需要权限: 删除消息 + 封禁用户[用于无权删除旧消息时兜底])

> Telegram 硬要求:bot 必须是频道管理员,才能用 `getChatMember` 查询任意用户的频道成员状态。

### 3. 确保群已关联到频道

在 Telegram 客户端: 频道 → 设置 → 讨论 → 选择评论群。未关联时 `/bind` 会报错。

### 4. 运行 bot

```bash
cp config.yaml.example config.yaml
# 编辑 config.yaml 填入 bot_token
make build
./bin/group-limit-bot --config ./config.yaml
```

或用环境变量:

```bash
BOT_TOKEN=xxx ./bin/group-limit-bot
```

### 5. 绑定

在评论群内(由群创建者执行):

```
/bind
```

bot 会自动读取群的 linked_chat_id 完成绑定。无需手填任何 ID。

## 可用命令(仅群创建者)

**群创建者**可执行 `/bind`、`/unbind`、`/status`;其他角色(管理员/成员)无权运行这些命令。匿名身份发送也会被拒绝,操作者须关闭匿名模式再执行。

| 命令 | 说明 |
|---|---|
| `/bind` | 在评论群内执行,自动绑定到群关联的频道 |
| `/unbind` | 解除当前群的绑定,清空缓存 |
| `/status` | 查看当前绑定、缓存命中、近 1h 删除计数、最近错误 |

## CLI 子命令

```bash
group-limit-bot                # 运行 bot(默认)
group-limit-bot --config PATH  # 指定配置文件运行
group-limit-bot version        # 打印版本 + 构建信息(也支持 --version)
group-limit-bot update         # 查询 GitHub Release,自动下载并原地替换二进制
group-limit-bot --help         # 显示帮助
```

`update` 会自动识别当前平台(linux/macOS × amd64/arm64),下载最新版,把旧二进制备份为 `<path>.bak`。如果是 systemd 托管,升级后需要重启服务:

```bash
sudo /data/group-limit-bot/group-limit-bot update
sudo systemctl restart group-limit-bot
```

## 配置项

| 键 | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `bot_token` | `BOT_TOKEN` | (必填) | `@BotFather` 颁发 |
| `db_path` | `BOT_DB_PATH` | `./bot.db` | SQLite 文件路径 |
| `cache_ttl` | `BOT_CACHE_TTL` | `30m` | 已关注者缓存 TTL |
| `log_level` | `BOT_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `allowlist` | `BOT_ALLOWLIST` | `[]` | 白名单 bot 的 user_id 列表,逗号分隔(环境变量形式) |
| `allow_anonymous_admin` | `BOT_ALLOW_ANONYMOUS_ADMIN` | `true` | 是否放行群匿名管理员 |

环境变量优先级高于 yaml。

> `bot_allowlist` / `BOT_BOT_ALLOWLIST` 仍可接受,但已弃用,将在下一版本移除。

## 行为说明(给用户公告用)

> 本群启用守卫机制。发言前请确保已关注频道 @xxx。
> 未关注用户的消息会被自动删除,关注后即可正常发言(生效有最多约 30 分钟延迟)。

## 故障与排查

- **bot 没反应**:确认 Privacy Mode 已 Disable;确认 bot 是群管理员。
- **`/bind` 报"未关联讨论频道"**:到频道设置里把这个群设为 discussion group。
- **`/bind` 报"请先将 bot 加为频道管理员"**:到频道管理员列表添加 bot。
- **全部消息都没被删(本该被删的也没删)**:查看日志,看是否持续 `getChatMember` 失败——bot 很可能失去了频道管理员身份。运行 `/status` 看 `recent_errors`。

## 隐私

bot 只读取自己收到的消息用于决策。不存储消息内容,仅存(group_id, user_id, 过期时间)三元组用于缓存。
