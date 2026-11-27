# 使用指南

本指南介绍日常聊天命令、角色记忆、主动跟踪、个人 MCP 连接及 SSH 看板。初次运行见[部署与更新](getting-started.md)，参数和默认值见[配置参考](configuration.md)。

- [角色记忆](#角色记忆)
- [主动闲聊与长期跟踪](#主动闲聊与长期跟踪)
- [个人 MCP](#个人-mcp)
- [定时任务](#定时任务)
- [自然语言提醒](#自然语言提醒)
- [Bot 命令](#bot-命令)
- [SSH Dashboard](#ssh-dashboard)

## 角色记忆

Bot 在对话中维持当前角色。记忆按访问权限和适用的聊天查找。你可以在普通对话中补充或纠正已有的记忆。在私聊中引用本人在超级群里发过的原消息，可同时纠正由该事件产生的群内推断。

在本人私聊发送 `/memory_share <群 ID> <结论原文>`，将一条私聊结论共享给指定群。Bot 会核实群组和成员身份。`/memory_unshare` 可使用相同参数撤回。撤回会影响之后的记忆读取，已经发出的 Telegram 消息仍在聊天记录中。

`/clear` 清除当前聊天的近期上下文和摘要。长期记忆继续留在 Memory Book。管理员可在本人私聊管理原始条目，具体命令见[命令表](#bot-命令)。

## 主动闲聊与长期跟踪

角色沿用当前 `SYSTEM_PROMPT`。可以用普通对话提出或调整关注，群聊中可以 @机器人 发起对话。角色会说明它能承接的部分和更新的去向。示例：

- 「等上游把修复 #41 发进稳定版，我就能撤补丁了。」部署者配置了对应的只读发布源时，角色可以记下这一关注。来源需要能够证明正式发布且该版本包含 #41。合入和预览进度可以作为观察依据，同一来源的同一版本按约定受众去重。
- 「等发行说明重写了 请私聊我。」更新只会发到发起人的私聊。Bot 需要能进入该私聊，并验证来源群及发起人的访问权限。仅靠个人白名单能在某个群里与 Bot 对话时，后台活动仍需要这个群本身可用。
- 「帮项目组盯稳定版，发到这个群，再拉代码跑验证。」已配置发布源时可以承接通知。后台只支持配置好的读源和已授权的日历创建。群成员可对共同事项提议「只报稳定版」。个人事项、账号和明确约定的管理范围按实际 Telegram 身份核验。
- 「我下周有一场手术。」角色可在原私聊安排一次适时的关心。默认至少等七天，明确的日期可用来安排其他时间。结果以本人回复为准。

| 在对话中说 | 生效的范围 |
|---|---|
| 「这周少找我闲聊，发布照常告诉我」 | 私聊的主动闲聊。群员也可以提出「本群这周少主动闲聊」，由角色按本群语境裁决 |
| 「这个聊天先别推跟踪更新」 | 当前会话的跟踪通知。后台仍可核实和执行已承接的动作 |
| 「这件事先留意，这周别推消息」 | 指定事项的通知。恢复时重新核实只读结果，持续关注会合并仍有意义的更新。一次性关注若尚未投递且旧结果已失效，会继续等待新变化 |
| 「这件事暂停」或「恢复这项跟踪」 | 暂停或按原约恢复该事项的检查、已承接动作和发送 |
| 「我们换库了，这个不用再盯了」 | 结束匹配的关注及其后续动作，保留已发生的结果 |

**频率与访问。** 私聊需要至少两条 Bot 实际收到的本人消息，空闲 12 小时后才考虑搭话。群需要观察至少两位成员的三条消息，空闲 48 小时后才考虑。角色可以保持安静，通常至多每天考虑一次。同会话两次主动开口至少间隔 72 小时，上一次未获回应就保持安静。在群内回复本 Bot 或用完整用户名提及它可解除等待。设为「减少」会拉长考虑间隔，设为「关闭」会停止主动闲聊。群隐私模式决定 Telegram 送达哪些消息，`CONTEXT_MODE=global` 可将 Bot 已收到的普通群消息加入原群上下文。旧部署在升级后收到新互动，才会建立主动闲聊所需的状态。

**运行条件。** 请持久化 `CHAT_DB_PATH`，机器人重启后会记住会话冷却、事项偏好和投递结果。后台发布或活动检查通常间隔一分钟，受 Bot 在线状态、来源、模型和配额影响。群自主活动只计入群会话，明确委托按原发起人和执行会话计费。主动文字投递由 Telegram 返回对应聊天及实际正文后确认。结果不明的尝试会保留待核实状态，重启后不会自动重发。原有 `/remind` 定时提醒照约投递。`ADMIN_ID=*` 关闭管理入口，普通会话仍可用自然语言调整自己的偏好和跟踪事项。

## 个人 MCP

获准用户可导入自己专属的 MCP 服务器。个人工具按账号隔离，默认在本人的私聊中可用。服务器配置保存在 bbolt，重启后按当前权限恢复连接。

启用[工具调用](configuration.md#工具调用与-mcp)后，可在本人私聊向 Bot 发送以下格式的 JSON。

```json
{
  "mcpServers": {
    "my-server": {
      "type": "streamablehttp",
      "url": "https://my-mcp-server.com",
      "headers": {
        "Authorization": "Bearer my-token"
      }
    }
  }
}
```

Bot 会自动识别 JSON 格式并导入，返回确认消息。可多次发送以追加或更新服务器配置。

要在指定群里查询并分享个人网络工具的结果，先在本人私聊发送 `/mcp_share <群 ID> <工具名> <JSON 参数>`。通过群成员核验后，由本人在该群发送 `/mcp_use <工具名> <JSON 参数>`，使用与授权一致的参数。回到私聊发送 `/mcp_unshare <群 ID> <工具名>` 可撤回授权。重启或重建该工具连接后需要重新授权。

**管理命令**：

| 命令 | 说明 |
|---|---|
| `/mcp_list` | 查看你的个人 MCP 服务器列表 |
| `/mcp_del <name>` | 删除指定名称的 MCP 服务器 |
| `/mcp_clear` | 清除你所有的个人 MCP 服务器 |

管理命令说明：`/mcp_list` 在摘要生成完成后会在每条服务器信息下方展示其功能摘要，便于快速了解该 server 的用途。

## 定时任务

定时任务按 `chat_id` 隔离保存：不同私聊、群聊之间互不共享。任务触发时，bot 会自动向 LLM 发送一条预设 `prompt`，然后走和普通聊天相同的回复链路，把结果发回当前聊天。

权限规则：

- `upsert`、`delete`、`pause`、`resume` 仅管理员可用
- `list`、`help`、`example` 可在当前聊天直接查看

### 创建或更新任务

在要接收结果的聊天中发送如下 JSON。

```json
{
  "schedule": {
    "action": "upsert",
    "id": "morning-brief",
    "name": "每日早报",
    "prompt": "总结今天需要关注的技术新闻",
    "time": {
      "cron": "0 9 * * *",
      "timezone": "Asia/Shanghai"
    },
    "context": false,
    "enabled": true
  }
}
```

字段说明：

- `action`: 操作类型，支持 `upsert`、`delete`、`list`、`pause`、`resume`
- `id`: 任务唯一标识，同一聊天内唯一
- `name`: 可选的展示名称
- `prompt`: 定时发送给 LLM 的内容
- `time.cron`: 标准 5 段 cron 表达式
- `time.timezone`: IANA 时区名，例如 `Asia/Shanghai`
- `context`: 可选，默认 `false`；设为 `true` 时会把当前聊天的滑窗上下文和摘要一并带给 LLM
- `enabled`: 可选，默认 `true`

### Schedule 命令

| 命令 | 说明 |
|---|---|
| `/schedule` | 显示 schedule 帮助 |
| `/schedule_new` | 交互式创建定时任务（仅管理员） |
| `/schedule help` | 显示 schedule 帮助 |
| `/schedule example` | 显示创建任务的 JSON 模板 |
| `/schedule list` | 查看当前聊天的任务列表 |
| `/schedule pause <id>` | 暂停指定任务（仅管理员） |
| `/schedule resume <id>` | 恢复指定任务（仅管理员） |
| `/schedule delete <id>` | 删除指定任务（仅管理员） |
| `/schedule_list` | `/schedule list` 的快捷别名 |
| `/schedule_example` | `/schedule example` 的快捷别名 |
| `/schedule_pause <id>` | `/schedule pause <id>` 的快捷别名 |
| `/schedule_resume <id>` | `/schedule resume <id>` 的快捷别名 |
| `/schedule_del <id>` | `/schedule delete <id>` 的快捷别名 |

### 常用示例

每 5 分钟执行一次：

```json
{
  "schedule": {
    "action": "upsert",
    "id": "test-task",
    "name": "测试定时任务",
    "prompt": "这是一个测试定时任务。请回复：定时任务执行成功。",
    "time": {
      "cron": "*/5 * * * *",
      "timezone": "Asia/Shanghai"
    },
    "context": false,
    "enabled": true
  }
}
```

## 自然语言提醒

与 `/schedule` 面向管理员的 cron+JSON 定义不同，`/remind` 面向所有允许用户，用自然语言创建一次性或重复提醒：

- 用户发送 `/remind 明天早上9点提醒我交报销`，机器人调用解析 LLM 将其转换为 `{fire_at, cron?, prompt}`，写入 `chat.db` 的 `user_reminders` bucket
- 提醒与 `/schedule` 使用同一个调度器。一次性提醒在执行后移除，投递结果未知时保留待确认记录。重复提醒按 cron 安排下次触发
- 到期时复用角色回复流程，将结果发回创建提醒的聊天
- **安全边界**：仅 `/remind` `/reminders` `/reminder_del` 三个命令能访问提醒存储，普通聊天和工具调用无法直接读写提醒存储
- **可见范围**：群聊中 `/reminders` 只列出本群聊内由当前用户创建的提醒；私聊中列出当前用户在任何聊天创建的全部提醒。提醒始终在创建所在的 chat 触发

### Reminder 命令

| 命令 | 说明 |
|---|---|
| `/remind <自然语言>` | 创建一次性或重复提醒 |
| `/reminders` | 列出自己的提醒（群聊仅限本群聊；私聊列出全部） |
| `/reminder_del <id>` | 删除指定 ID 的提醒（仅能删自己的） |
| `/reminderd <id>` | `/reminder_del` 的别名 |

### Reminder 示例

```text
/remind 明天早上9点提醒我交报销
/remind 10秒后说 hello
/remind 每周一早上9点提醒开会
```

解析模型和默认时区见[提醒配置](configuration.md#自然语言提醒)。

## Bot 命令

| 命令 | 说明 |
|---|---|
| `/start` | 显示欢迎信息 |
| `/admin` | 显式管理员在私聊中打开配置面板 |
| `/clear` | 清除当前聊天的历史、待摘要内容和摘要，并使清空前启动的对话工作失效。长期记忆保留。 |
| `/summary` | 查看当前对话的摘要内容 |
| `/speach [on\|off\|toggle\|status]` | 管理员切换当前聊天的 TTS 模式 |
| `/displayp` | 在本人私聊查看旧画像 |
| `/clearp` | 在本人私聊清除旧画像 |
| `/memory list [游标]`、`show <ID> [分段]`、`edit <ID> <版本> <结论>`、`delete <ID> <版本>` | 管理员本人私聊管理原始记忆 |
| `/memory_share <群 ID> <结论原文>`、`/memory_unshare <群 ID> <结论原文>` | 在本人私聊确认或撤回指定群的单条结论授权 |
| `/sticker [status\|reload\|<label/alias>]` | 贴纸命令：查看状态、重载规则（仅管理员）、或按标签手动发送 |
| `/mcp_list` | 查看个人 MCP 服务器列表 |
| `/mcp_del <name>` | 删除指定 MCP 服务器 |
| `/mcp_clear` | 清除所有个人 MCP 服务器 |
| `/schedule` | 查看 schedule 帮助和子命令 |
| `/schedule_new` | 交互式创建定时任务（仅管理员） |
| `/schedule_list` | 查看当前聊天的定时任务列表 |
| `/schedule_example` | 查看定时任务 JSON 模板 |
| `/schedule_pause <id>` | 暂停指定定时任务（仅管理员） |
| `/schedule_resume <id>` | 恢复指定定时任务（仅管理员） |
| `/schedule_del <id>` | 删除指定定时任务（仅管理员） |
| `/remind <自然语言>` | 创建一次性或重复提醒（所有允许用户） |
| `/reminders` | 列出自己的提醒 |
| `/reminder_del <id>` | 删除自己的提醒（`/reminderd` 为别名） |
| `/quota_reset [me\|here\|user <id>\|chat <id>]` | 管理员重置今日配额计数；同时清空内存与 `usage_daily` 对应行 |

### BotFather 命令列表

下面这份可以直接发给 BotFather 的 `/setcommands`：

```text
start - 显示欢迎信息
clear - 清除当前对话上下文和摘要
summary - 查看当前对话摘要
speech - 切换当前聊天的语音模式
sticker - 查看贴纸状态或发送贴纸
displayp - 在本人私聊查看旧画像
clearp - 在本人私聊清除旧画像
mcp_list - 查看个人 MCP 服务器列表
mcp_del - 删除指定 MCP 服务器
mcp_clear - 清空个人 MCP 服务器
schedule - 查看定时任务帮助
schedule_new - 交互式创建定时任务
schedule_list - 查看当前聊天的定时任务列表
schedule_example - 查看定时任务 JSON 模板
schedule_pause - 暂停指定定时任务
schedule_resume - 恢复指定定时任务
schedule_del - 删除指定定时任务
remind - 自然语言创建一次性或重复提醒
reminders - 列出自己的提醒
reminder_del - 删除自己的提醒
quota_reset - 管理员重置今日配额
admin - 管理员配置面板
```

说明：

- `speech` 和代码里的 `/speach` / `/speech` 都可用，推荐在 BotFather 中只展示 `speech`
- BotFather 的命令描述建议尽量简短，否则容易超长

### 重置每日配额

```text
/quota_reset                 查看用法
/quota_reset me              重置当前发送者自己的今日 user 计数
/quota_reset here            重置当前聊天的今日 chat 计数
/quota_reset user <user_id>  重置指定用户
/quota_reset chat <chat_id>  重置指定聊天
```

## SSH Dashboard

项目内置一个只读 SSH TUI 看板，用于查看全局概览、按 `userID` 聚合的用户详情、按聊天维度的定时任务，以及详细事件流。

### Dashboard 能看到什么

- `Overview`：最近窗口请求数、成功率、总 token、热点聊天、最近活跃用户/聊天
- `Users`：左侧用户列表，右侧展示 `userID`、`username`、`displayName`、profile、个人 MCP、个人 usage 汇总
- `Schedules`：按聊天查看定时任务、下次执行时间、上次执行时间、最近错误
- `Logs`：比进程终端更细的结构化事件流，包括对话、usage、tool、MCP、schedule、profile、config 等事件

### 启用步骤

1. 创建数据目录并生成服务端 host key。

   ```bash
   mkdir -p ./data
   ssh-keygen -t ed25519 -f ./data/dashboard_ssh_ed25519 -N ""
   ```

2. 创建 `authorized_keys` 白名单。

   ```bash
   touch ./data/dashboard_authorized_keys
   chmod 600 ./data/dashboard_authorized_keys
   ```

3. 把允许登录的客户端公钥写进去，每行一个。

   ```text
   ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... your-name@machine
   ```

4. 在 `./data/config.yaml` 中开启看板。

   ```yaml
   DASHBOARD_SSH_ENABLED: true
   DASHBOARD_SSH_LISTEN: :23234
   DASHBOARD_SSH_HOST_KEY_PATH: ./data/dashboard_ssh_ed25519
   DASHBOARD_SSH_AUTHORIZED_KEYS_PATH: ./data/dashboard_authorized_keys
   ```

5. 重启 Bot 进程或容器。

### 本地连接示例

```bash
ssh -i ~/.ssh/id_ed25519 -p 23234 dashboard@127.0.0.1
```

说明：

- `-i` 指向的是与你写入 `authorized_keys` 的那条公钥相对应的私钥
- 用户名只是 SSH 会话字段，占位即可，例如 `dashboard`
- Dashboard 使用独立的 SSH 公钥认证。`ADMIN_ID` 可以留空。生效名单中有一项为 `*` 时，Dashboard 会停止监听并断开现有连接。
- 如果通过 Docker/Podman 运行，需要 compose 暴露 `23234:23234`，项目中的两个 compose 文件已包含该映射

### TUI 快捷键

- `Tab` / `Shift+Tab`：切换 Tab
- `h` / `l`：切换 Tab
- `j` / `k`：在列表中移动
- `r`：手动刷新
- `q`：退出

所有 SSH 参数见[配置参考](configuration.md#ssh-dashboard)。

## 架构简述

```
用户语音 → handleVoice()
            ├─ 权限检查: isAllowed()
            ├─ STT 未配置 → 静默忽略
            ├─ 下载 OGG/OPUS 流 → STT API (/v1/audio/transcriptions)
            ├─ STT_DISPLAY=true → 回复 🎙️ <转写内容>
            └─ 以转写文本构建 userMsg → startRoleReply()

用户图片 → handlePhoto()
            ├─ 权限检查: isAllowed()
            ├─ VISION_ENABLED=false → 静默忽略
            ├─ 群聊: 检查 caption 中的 @mention（或 AUTO_DETECT 判定相关）
            ├─ 下载最大尺寸原图 → 校验大小不超过 VISION_MAX_IMAGE_BYTES
            ├─ 嗅探 MIME → base64 编码 → data URL
            └─ 构建 MultiContent userMsg [text + image_url] → startRoleReply()

用户文档 → handleDocument()
            ├─ 权限检查: isAllowed()
            ├─ DOCUMENT_ENABLED=false → 静默忽略
            ├─ 扩展名白名单校验（DOCUMENT_ALLOWED_EXTS）→ 不在白名单 → 拒绝
            ├─ 群聊: 检查 caption 中的 @mention（或 AUTO_DETECT 判定相关）
            ├─ 下载 → io.LimitReader 二次加界 → 校验不超过 DOCUMENT_MAX_BYTES
            ├─ 按格式提取文本：PDF 使用 pdftotext，部分 Office 文档使用 pandoc，表格和纯文本在进程内处理
            ├─ 按 rune 硬截断到 DOCUMENT_MAX_TEXT_CHARS
            └─ 构建 userMsg（Content 含文件元信息 + 截断文本）→ startRoleReply()

用户消息 → handleText()
            ├─ 权限检查: isAllowed()
            │   ├─ 私聊: 检查 ALLOWED_USERS
            │   └─ 群聊: 检查 ALLOWED_GROUPS 或 ALLOWED_USERS
            │           └─ 未授权 → 静默忽略
            ├─ MCP JSON 自动检测
            │   └─ 消息以 { 开头且包含 "mcpServers" → 导入用户 MCP 配置
            ├─ 私聊: 直接处理
            └─ 群聊: 检查 @mention
                     ├─ 被 @ → 处理
                     ├─ 未被 @ + AUTO_DETECT → isRelevant() 判断
                     │                         ├─ 相关 → 处理
                     │                         └─ 不相关 → 忽略
                     └─ 未被 @ → CONTEXT_MODE=global 时存入上下文，不回复

处理流程:
  1. 从 Telegram 更新取得真实发送者和聊天，读取本轮角色设定。
  2. 按受众和授权筛选 Memory Book 条目，再选出相关记忆。
  3. 将角色设定作为系统消息，将可见记忆和本聊天摘要作为参考资料，附上近期历史和当前消息。
  4. Eino 按本轮权限提供工具，执行角色选择的调用并把结果交回模型。
  5. 按会话设置投递文字、语音或贴纸。将用户消息和已送达的回复正文写入本聊天历史。
  6. 有待摘要的历史且达到阈值时更新摘要。对符合学习条件的本轮发言，后台评估是否更新 Memory Book。
```

### 技术依赖

- [Eino](https://github.com/cloudwego/eino) 用于模型调用与工具编排
- [telebot v3](https://github.com/tucnak/telebot) — Telegram Bot 框架
- [go-openai](https://github.com/sashabaranov/go-openai) — OpenAI Go SDK
- [godotenv](https://github.com/joho/godotenv) — .env 文件加载
- [bbolt](https://github.com/etcd-io/bbolt) — 嵌入式 key-value 数据库（用户画像 + MCP 配置持久化）
- [mcp-go](https://github.com/mark3labs/mcp-go) — MCP (Model Context Protocol) Go 客户端
