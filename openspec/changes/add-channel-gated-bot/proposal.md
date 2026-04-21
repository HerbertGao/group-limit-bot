## 为什么

Telegram 频道的 linked 讨论群(评论区)经常被未关注频道的用户刷广告、灌水,伤害关注者的交流体验。现有的通用反垃圾 bot 无法基于"是否关注了绑定频道"这个业务规则过滤——而这恰恰是运营者区分"核心受众"与"路人"最天然的门槛。我们需要一个轻量、自托管、一人一 bot 即可守护多组"频道 ↔ 评论群"配对的守卫工具。

## 变更内容

本项目从零起步,交付一个 Go 实现的 Telegram 守卫 bot:

- 新增**懒校验删除**机制:用户在绑定评论群发言时,bot 实时(带缓存)查询其在绑定频道的成员状态,未关注者的消息静默删除。
- 新增**一 bot 多绑定**支持:单个 bot 进程可同时服务多对(频道, 评论群),用 `group_id → channel_id` 数据表路由。
- 新增**自动绑定命令**:群内管理员通过 `/bind` 触发,bot 自动读取群的 `linked_chat_id` 完成配对,避免手工填错 ID。
- 新增**短期成员缓存**:仅缓存"已通过"结果,TTL 30 分钟,减轻 Bot API 限速压力;未通过结果不缓存,保证用户关注后立刻生效。
- 新增**编辑消息重校验**:`edited_message` 走同样流程,防止"先发正常话通过再编辑成广告"。
- 新增**sender_chat 一律删除**策略:排除绑定频道自身转发的根帖后,其他频道以频道身份发言的消息一律删除,防止刷频道号。
- 新增**运营辅助命令**:`/status`、`/unbind` 提供绑定列表、缓存命中率、近期删除量等观测数据。

## 功能 (Capabilities)

### 新增功能

- `channel-gating`: 判定某条消息是否应被删除的核心业务规则——包括白名单短路、频道成员状态查询、缓存策略、编辑消息重校验、sender_chat 处理。
- `group-binding`: 评论群与频道的绑定生命周期——`/bind` 自动识别 linked_chat_id、`/unbind`、多绑定持久化、权限校验(调用者必须是群管理员)。
- `bot-runtime`: bot 进程的运行时骨架——Telegram Bot API 客户端、长轮询、update 路由、panic 恢复、结构化日志、配置加载、优雅关停。
- `admin-commands`: 群管理员可用的运营命令及其权限模型——`/bind`、`/unbind`、`/status`,调用者身份校验,错误反馈格式。

### 修改功能

无。这是项目首个变更,没有已存在的规范可修改。

## 影响

- **代码**: 新建 Go 模块,目录结构从零搭建(`cmd/bot`、`internal/gating`、`internal/binding`、`internal/store`、`internal/telegram`、`internal/config`)。
- **依赖**: 引入 `github.com/mymmrac/telego`(Telegram Bot API 客户端)、`modernc.org/sqlite`(纯 Go SQLite 驱动)、`log/slog`(标准库结构化日志)。
- **外部系统**: 依赖 Telegram Bot API;bot 必须被手动添加为目标群和目标频道的管理员,且启用 Privacy Mode 关闭(否则收不到群消息)。
- **部署**: 单二进制 + 单个 SQLite 文件;配置通过环境变量或 `config.yaml` 注入 bot token;长轮询无需公网 HTTPS 入口。
- **运维**: 日志带 `chat_id` 维度;需要监控 Telegram 429 限速响应并告警;SQLite 文件需要定期备份(丢失 = 所有绑定关系丢失,但无法直接恢复,只能管理员重新 `/bind`)。
