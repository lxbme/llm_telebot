# 配置参考

本文记录配置文件、环境变量、参数默认值及管理员私聊热修改。首次部署见[部署与更新](getting-started.md)，日常操作和命令见[使用指南](usage.md)。

- [配置文件与优先级](#配置文件与优先级)
- [核心配置](#核心配置)
- [语音播报](#语音播报)、[语音输入](#语音输入)、[图片理解](#图片理解)、[文档理解](#文档理解)和[贴纸策略](#贴纸策略)
- [群聊行为](#群聊行为配置)、[访问控制](#访问控制)与[管理员配置](#管理员配置)
- [角色记忆](#角色记忆)与[对话摘要](#对话摘要)
- [工具调用与 MCP](#工具调用与-mcp)
- [自然语言提醒](#自然语言提醒)、[每日用量配额](#每日用量配额)和 [SSH Dashboard](#ssh-dashboard)

## 配置文件与优先级

本文档统一推荐使用 `./data/config.yaml` 作为主配置文件，系统环境变量优先于 `.env` 中的同名变量，非空的生效环境变量覆盖 YAML 中的同名项。

配置优先级：

```text
系统环境变量 > .env > YAML 配置 > 内置默认值
```

推荐做法：

- 将大部分业务配置写在 `./data/config.yaml`
- 将 API Key、Token、容器路径覆盖等写在 `.env`
- `/admin` 会把修改写回当前 `CONFIG_FILE` 指向的文件；若该项仍被环境变量覆盖，则重启后会恢复为环境变量的值

### 配置文件位置

- 本地直接运行时，若未设置 `CONFIG_FILE`，默认配置文件路径为 `./data/config.yaml`（代码常量：`DefaultConfigFilePath`，定义于 `internal/app/config_file.go`）
- 容器镜像内默认配置文件路径为 `/app/data/config.yaml`
- 可以用 `CONFIG_FILE` 指定配置文件。容器中填写容器内的路径。完整的配置模板见 [config.yaml.example](../config.yaml.example) 和 [.env.example](../.env.example)

### `/admin` 热修改说明

- 仅管理员私聊可用
- 发送 `/admin` 后，Bot 会列出可编辑的配置项及编号
- 回复编号后按面板提示提交新值。密钥等敏感配置要使用面板给出的完整提交命令。可保存的配置会写回 `CONFIG_FILE` 指向的文件
- 两步消息都带 `cancel` 按钮，可返回上一步或退出
- 输入 `<empty>` 可清空字符串或列表配置
- `TELEGRAM_BOT_TOKEN` 修改后会写入运行时与配置文件，但建议重启进程后再确认轮询连接状态

## 核心配置

| 环境变量 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `OPENAI_API_BASE` | 否 | `https://api.openai.com/v1` | OpenAI 兼容 API 的 Base URL |
| `OPENAI_API_KEY` | **是** | — | API 密钥 |
| `OPENAI_MODEL` | 否 | `gpt-4o` | 模型名称 |
| `TELEGRAM_BOT_TOKEN` | **是** | — | Telegram Bot Token |
| `CONFIG_FILE` | 否 | `DefaultConfigFilePath`（`./data/config.yaml`） | YAML 主配置文件路径；容器镜像中默认是 `/app/data/config.yaml` |
| `BOT_USERNAME` | 否 | 自动获取 | 机器人用户名（带 @ 前缀），用于群聊中检测 @提及 |
| `SYSTEM_PROMPT` | 否 | `You are a helpful assistant.` | 系统提示词 |
| `CONTEXT_MAX_MESSAGES` | 否 | `20` | 每个对话保留的最大消息数（滑动窗口） |
| `EINO_MODEL_RETRY_ENABLED` | 否 | `false` | 主模型遇到瞬时错误时最多重试一次。通过环境变量或 `.env` 设置。 |
| `EINO_FALLBACK_MODEL` | 否 | — | 主模型遇到瞬时错误后可尝试一次的备用模型，使用同一个 API 地址和密钥。通过环境变量或 `.env` 设置。 |
| `CHAT_DB_PATH` | 否 | `./data/chat.db` | 保存聊天记录、摘要、记忆、任务和用量的数据库。 |
| `MAX_TOKENS` | 否 | `0` | 每次回复的最大 token 数，0 表示不限。新一代模型（o1/o3/o4 系列及 gpt-5+）会自动切换为 `max_completion_tokens` 字段，并自动清除 `temperature`、`top_p` 等不受支持的采样参数，无需额外配置 |

## SSH Dashboard

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `DASHBOARD_SSH_ENABLED` | `false` | 是否启用内嵌 SSH TUI 看板 |
| `DASHBOARD_SSH_LISTEN` | `:23234` | Dashboard SSH 监听地址 |
| `DASHBOARD_SSH_HOST_KEY_PATH` | `./data/dashboard_ssh_ed25519` | SSH 服务端 host key 私钥路径 |
| `DASHBOARD_SSH_AUTHORIZED_KEYS_PATH` | `./data/dashboard_authorized_keys` | 允许登录的客户端公钥白名单文件 |
| `DASHBOARD_SSH_IDLE_TIMEOUT` | `1h` | 单个 SSH session 的空闲超时 |
| `DASHBOARD_SSH_MAX_SESSIONS` | `8` | 同时允许的最大 Dashboard 会话数 |

看板启用和连接步骤见[使用指南](usage.md#ssh-dashboard)。

- `HOST_KEY` 是服务器证明自身身份的私钥文件，应该保密
- `AUTHORIZED_KEYS` 是客户端公钥白名单，格式与 OpenSSH 的 `authorized_keys` 一致
- 客户端连接时仍需要本地持有与公钥匹配的私钥，服务器不会保存你的私钥

## 语音播报

用于 `/speach` 模式。该模式只能由管理员在当前私聊/群聊内开启或关闭；开启后，bot 会向 LLM 额外注入“简短回答”的约束，并在文本回复后追加发送一条 TTS 语音消息。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `VOLCENGINE_TTS_APP_ID` | — | 火山引擎 APP ID |
| `VOLCENGINE_TTS_ACCESS_KEY` | — | 火山引擎 Access Token |
| `VOLCENGINE_TTS_RESOURCE_ID` | `seed-tts-1.0` | 资源 ID |
| `VOLCENGINE_TTS_SPEAKER` | `zh_female_shuangkuaisisi_moon_bigtts` | 音色 ID |
| `VOLCENGINE_TTS_AUDIO_FORMAT` | `mp3` | 火山原始输出格式，目前支持 `mp3` / `wav` / `aac` |
| `VOLCENGINE_TTS_SAMPLE_RATE` | `24000` | 输出采样率 |
| `VOLCENGINE_TTS_SPEECH_RATE` | `0` | 语速，范围 `-50` 到 `100` |
| `VOLCENGINE_TTS_SEND_TEXT` | `true` | `/speach` 模式下是否同时保留文字回复；设为 `false` 时默认只发语音，若语音失败会回退显示文字 |

> 当前实现使用火山引擎 SSE 单向流式接口：[`/api/v3/tts/unidirectional/sse`](https://www.volcengine.com/docs/6561/1598757?lang=zh#_3-sse%E6%A0%BC%E5%BC%8F%E6%8E%A5%E5%8F%A3%E8%AF%B4%E6%98%8E)。bot 会在本地通过 `ffmpeg` 将火山返回的音频转成 Telegram 语音所需的 `ogg/opus` 后再发送。

## 语音输入

接收 Telegram 语音消息，通过任何兼容 OpenAI `/v1/audio/transcriptions` 接口的端点进行转写。转写结果会以普通用户文本消息的形式进入完整对话流程（历史记录、工具调用、TTS、画像提取等）。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `STT_ENABLED` | `false` | 是否接收并转写语音消息 |
| `STT_API_BASE` | — | STT 端点 Base URL（留空使用 OpenAI 官方默认） |
| `STT_API_KEY` | — | STT 端点 API Key（必填，当 `STT_ENABLED=true` 时） |
| `STT_MODEL` | `whisper-1` | STT 模型名称，例如 `whisper-1`、`gpt-4o-transcribe` |
| `STT_DISPLAY` | `true` | 转写成功后是否以 `🎙️ <转写内容>` 回复用户；设为 `false` 则静默处理 |

说明：

- STT 使用独立 API Key，**不会**回退到主模型配置，需单独填写
- Telegram 语音消息为 OGG/OPUS 格式，直接以流式传输发送给 STT 接口，无本地落盘
- 语音消息同样受 `ALLOWED_USERS` / `ALLOWED_GROUPS` 白名单控制；未配置 STT 时，语音消息被静默忽略
- 可通过 `/admin` 热修改所有 STT 配置项，无需重启

## 图片理解

接收 Telegram 图片消息（含可选 caption），下载最大尺寸的原图后按 OpenAI Vision 协议封装为 base64 `image_url` data URL，与 caption 一起作为 `MultiContent` 发送给主对话模型。回复流程与文字消息完全一致：流式输出、历史记忆、工具调用、画像提取、贴纸/TTS 后处理均自动复用。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `VISION_ENABLED` | `false` | 是否接收并理解图片消息；关闭时图片被静默忽略 |
| `VISION_MAX_IMAGE_BYTES` | `5242880` | 单张图片的最大字节数（默认 5 MiB），超出会拒收并提示用户 |

说明：

- 视觉能力由主对话模型提供，**复用** `OPENAI_API_BASE` / `OPENAI_API_KEY` / `OPENAI_MODEL`，因此需要将主模型配置为支持视觉的模型（如 `gpt-4o`、`gpt-4o-mini`、`qwen-vl-max` 等）
- 图片以 base64 data URL 形式直接嵌入请求，不依赖外部 CDN；MIME 类型由本地嗅探得出
- 图片消息同样受 `ALLOWED_USERS` / `ALLOWED_GROUPS` 白名单控制
- 群聊中遵循与文字消息相同的 @ 提及规则：caption 必须包含 `BOT_USERNAME`（或被 `AUTO_DETECT` 判定为相关）才会触发回复
- 历史记录中的图片消息会以多模态格式持久化到 `chat.db`，后续轮次仍可引用上文图片
- 可通过 `/admin` 热修改 `VISION_ENABLED` 与 `VISION_MAX_IMAGE_BYTES`，无需重启

## 文档理解

接收 Telegram 文件上传。`internal/app/document.go` 内部维护一个按扩展名路由的 parser 注册表：

| 扩展名 | Parser | 输出格式 |
|---|---|---|
| `pdf` | 本地 `pdftotext` (poppler-utils)，stdin/stdout 管道 | 带版式的纯文本 |
| `docx` / `odt` / `pptx` / `epub` | 本地 `pandoc`，临时文件 → `gfm` (`--wrap=none`) | GitHub-flavored markdown，保留标题/列表/表格/强调 |
| `xlsx` / `xlsm` | 进程内 `github.com/xuri/excelize/v2` | 每张 sheet 一个 `## Sheet: <name>` 段 + TSV 行 |
| 其他白名单扩展（txt/md/源代码…） | 直接按 UTF-8 读取（非法字节替换为 `U+FFFD`） | 原文 |

提取后的文本按 rune 数硬截断到 `DOCUMENT_MAX_TEXT_CHARS`，再连同文件元信息（文件名、扩展名、字节数）与用户 caption 一并作为普通 `Content` 注入主对话流程。后续历史、画像、工具调用完全复用现有管线。

**前置条件**：

- 想接收 PDF → 安装 `poppler-utils`（提供 `pdftotext`）
- 想接收 docx/odt/pptx/epub → 安装 `pandoc`（Alpine: `apk add pandoc`）
- 想接收 xlsx/xlsm → 无系统依赖（excelize 是 Go 库，已编译进二进制）
- 仅使用纯文本/源代码时无需任何额外安装

仓库自带的 `Dockerfile` 已经在运行镜像中预装 `poppler-utils` 与 `pandoc`，开箱即可启用所有 parser。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `DOCUMENT_ENABLED` | `false` | 是否接收并理解文档上传；关闭时文档被静默忽略 |
| `DOCUMENT_MAX_BYTES` | `5242880` | 单个文件的原始字节上限（默认 5 MiB），超出直接拒绝 |
| `DOCUMENT_MAX_TEXT_CHARS` | `20000` | 注入到 LLM 消息的文本长度上限（按 rune 计），超出硬截断并追加 `[... truncated, original N chars]` 标记 |
| `DOCUMENT_ALLOWED_EXTS` | 见 `config.yaml.example` | 允许的扩展名白名单（小写、无前缀点，英文逗号分隔）。增删后缀即可单独开关任一格式 |
| `DOCUMENT_PDFTOTEXT_CMD` | `pdftotext` | PDF 文本提取命令（PATH 名或绝对路径） |

说明：

- 启用某种 office 格式只需把后缀加进 `DOCUMENT_ALLOWED_EXTS`；如果对应的命令（`pandoc`）未安装，用户上传时会收到清晰的 "pandoc not installed" 错误，而不会让服务崩溃
- xlsx parser 内置两道安全闸：累计输出 ≤ 4 MiB 与 ≤ 50000 行，防止 zip-bomb；触发后会附加 `[... xlsx parser stopped early at safety cap]` 标记
- 扫描版 PDF（仅含图片、无文字层）会被 `pdftotext` 返回空串，机器人回复"文档不包含可提取文本"
- 下载使用 `io.LimitReader` 二次加界，即使 Telegram 汇报的 FileSize 不可信也不会 OOM
- 群聊遵循与图片/文字相同的 @ 提及规则：caption 必须包含 `BOT_USERNAME`（或被 `AUTO_DETECT` 判定为相关）才会触发回复
- 文档消息同样受 `ALLOWED_USERS` / `ALLOWED_GROUPS` 白名单控制
- 旧式 Office 二进制格式（`.doc` / `.xls` / `.ppt`）暂未支持，需要的话自行注册 parser（见 `documentParsers`）
- 可通过 `/admin` 热修改全部 `DOCUMENT_*` 配置项，无需重启

## 贴纸策略

贴纸能力采用“回复后处理”设计：先完成文本/语音主流程，再决定是否发送贴纸。策略优先使用关键词规则，规则未命中时可选模型辅助选标签。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `STICKER_ENABLED` | `false` | 是否启用贴纸策略 |
| `STICKER_MODE` | `append` | 贴纸模式：`off` / `append` / `replace` / `command_only` |
| `STICKER_PACK_NAME` | `""` | 贴纸包业务别名（预留字段） |
| `STICKER_RULES_PATH` | `./data/sticker_rules.json` | 贴纸规则文件路径（JSON） |
| `STICKER_SEND_PROBABILITY` | `0.25` | 自动贴纸触发概率，范围 `0~1` |
| `STICKER_MAX_PER_REPLY` | `1` | 每次回复最多发送贴纸数量 |
| `STICKER_WITH_SPEECH` | `true` | 语音模式开启时是否仍允许发贴纸 |
| `STICKER_ALLOWED_CHATS` | `""` | 允许启用贴纸策略的 chat_id 白名单（逗号分隔，空表示全部） |
| `STICKER_MODEL_ENABLED` | `false` | 是否启用模型辅助选贴纸标签 |
| `STICKER_MODEL_BASE` | `OPENAI_API_BASE` | 贴纸策略模型 API Base（留空回退主配置） |
| `STICKER_MODEL_KEY` | `OPENAI_API_KEY` | 贴纸策略模型 API Key（留空回退主配置） |
| `STICKER_MODEL` | `OPENAI_MODEL` | 贴纸策略模型名称（留空回退主配置） |

贴纸策略模型回退规则：

- `STICKER_MODEL_BASE` 为空 -> 使用 `OPENAI_API_BASE`
- `STICKER_MODEL_KEY` 为空 -> 使用 `OPENAI_API_KEY`
- `STICKER_MODEL` 为空 -> 使用 `OPENAI_MODEL`
- 三项都为空时，策略层复用主模型客户端

## 贴纸规则文件格式

默认值为 `./data/sticker_rules.json`，容器化场景可直接挂载 `./data` 目录复用同一路径。

示例：

```json
{
  "label_stickers": {
    "celebrate": ["<telegram_sticker_file_id_1>"],
    "agree": ["<telegram_sticker_file_id_2>"],
    "comfort": ["<telegram_sticker_file_id_3>"]
  },
  "keyword_to_label": {
    "恭喜": "celebrate",
    "收到": "agree",
    "抱歉": "comfort"
  },
  "command_to_label": {
    "yay": "celebrate",
    "ok": "agree",
    "sorry": "comfort"
  }
}
```

字段说明：

- `label_stickers`: 逻辑标签到 Telegram `file_id` 列表的映射（至少要有一个有效 `file_id` 才能发）
- `keyword_to_label`: 关键词匹配规则（命中后选择对应标签）
- `command_to_label`: `/sticker <payload>` 别名映射（例如 `/sticker yay`）

## 群聊行为配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `CONTEXT_MODE` | `at` | `at` = 仅 @消息作为上下文；`global` = 所有群聊消息作为上下文 |
| `AUTO_DETECT` | `false` | `true` = 自动判断未 @消息 是否与机器人相关并回复 |

## 访问控制

当 `ALLOWED_USERS` 和 `ALLOWED_GROUPS` 均未设置时，bot 对所有人开放。设置后，只有白名单中的用户/群组可以使用 bot。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `ALLOWED_USERS` | — | 允许使用 bot 的用户 ID 列表，逗号分隔。适用于私聊；群聊中该用户也被放行 |
| `ALLOWED_GROUPS` | — | 允许使用 bot 的群组/超级群组 ID 列表，逗号分隔。群内所有成员均可使用 |

> **提示：** 获取用户/群组 ID 的方法：向 [@userinfobot](https://t.me/userinfobot) 发送消息可获取用户 ID；将 [@RawDataBot](https://t.me/RawDataBot) 加入群组可获取群组 ID（通常为负数）。
>
> 示例：`ALLOWED_USERS=123456789,987654321`  `ALLOWED_GROUPS=-1001234567890`

## 管理员配置

管理员可以通过 JSON 导入 command-based (stdio) MCP 服务器。非管理员用户只能添加 HTTP/SSE 类型的远程 MCP 服务器。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `ADMIN_ID` | — | 逗号分隔的明确 Telegram 管理员用户 ID，留空表示没有 Telegram 管理员 |

`/admin` 拒绝保存包含 `*` 的管理员名单。如果启动或重载时生效的环境变量或 YAML 配置仍含通配符，Bot 会停用所有管理入口、现有管理编辑会话、管理员配额豁免和 SSH Dashboard。普通对话和用户的个人操作继续按原访问规则运行。在生效的配置源中改为明确的 Telegram 用户 ID，再重载或重启即可恢复管理。

## 自动检测独立模型（可选）

当 `AUTO_DETECT=true` 时，可为相关性判断配置独立的轻量模型，节省主模型的 token 消耗。未设置时，回退到主模型配置。

| 环境变量 | 回退值 | 说明 |
|---|---|---|
| `AUTO_DETECT_API_BASE` | `OPENAI_API_BASE` | 检测模型的 API 地址 |
| `AUTO_DETECT_API_KEY` | `OPENAI_API_KEY` | 检测模型的 API Key |
| `AUTO_DETECT_MODEL` | `OPENAI_MODEL` | 检测模型名称（推荐使用 `gpt-4o-mini` 等轻量模型） |

## 角色记忆

角色由当前 `SYSTEM_PROMPT` 定义，新一轮对话会采用热更新后的设定。角色在普通回复后评估本轮是否值得长期记住，学习结果存入 `CHAT_DB_PATH`。私聊事实默认只用于本人私聊，群内事实默认只用于原群。对同一 Telegram 用户的熟悉程度可以延续到其他聊天。保存、纠正和分享记忆的操作见[使用指南](usage.md#角色记忆)。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `PROFILE_ENABLED` | `true` | 新部署默认开启自动学习。已有配置中的 `false` 继续关闭自动学习和旧画像命令。已存记忆仍按受众可见。 |
| `PROFILE_DB_PATH` | `./data/profiles.db` | 旧画像数据库，保留本人私聊的 `/displayp` 与 `/clearp`。旧画像不进入角色提示。 |
| `PROFILE_EXTRACT_EVERY` | `3` | 兼容旧配置，旧提取间隔已停用。角色评估每次完成的合格对话，一次最多记一条，同时最多八次后台评估。 |
| `PROFILE_API_BASE` | `OPENAI_API_BASE` | 角色记忆评估模型的 API 地址 |
| `PROFILE_API_KEY` | `OPENAI_API_KEY` | 角色记忆评估模型的 API Key |
| `PROFILE_MODEL` | `OPENAI_MODEL` | 角色记忆评估模型名称 |

## 对话摘要

当滑动窗口溢出时，溢出的消息不会丢失，而是被后台 LLM 调用压缩成一段摘要。每次发给 LLM 的上下文结构为：

```
[角色设定] + [受众可见记忆与本聊天摘要] + [滑窗内原始对话] + [当前用户消息]
```

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `SUMMARY_ENABLED` | `true` | 是否启用对话摘要 |
| `SUMMARY_MIN_OVERFLOW` | `6` | 累积多少条溢出消息后才触发一次摘要（避免频繁调用） |
| `SUMMARY_API_BASE` | `OPENAI_API_BASE` | 摘要模型的 API 地址 |
| `SUMMARY_API_KEY` | `OPENAI_API_KEY` | 摘要模型的 API Key |
| `SUMMARY_MODEL` | `OPENAI_MODEL` | 摘要模型名称（推荐轻量模型） |

## 工具调用与 MCP

启用后，LLM 可自主判断是否需要调用注册的工具，并根据工具返回结果生成最终回复。

### 全局 MCP 配置

通过 JSON 配置文件接入远程 MCP 服务器，所有用户共享这些工具。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `TOOLS_ENABLED` | `false` | 是否启用工具调用 |
| `TOOLS_MAX_ITERATIONS` | `5` | 每次请求最多允许的工具调用轮数（防止无限循环） |
| `MCP_CONFIG_PATH` | — | 全局 MCP 服务器 JSON 配置文件路径。仅替换同路径文件后，重启 Bot 或触发工具连接重建才会读取新内容。更改配置路径会触发重建。 |
| `USER_MCP_DB_PATH` | `./data/user_mcp.db` | 用户动态 MCP 配置的 bbolt 数据库路径 |

**全局配置文件示例** (`mcp_config.json`)：

```json
{
  "mcpServers": {
    "mcd-mcp": {
      "type": "streamablehttp",
      "url": "https://mcp.mcd.cn",
      "headers": {
        "Authorization": "Bearer YOUR_MCP_TOKEN"
      }
    },
    "local-tool": {
      "type": "stdio",
      "command": "/usr/local/bin/mytool",
      "args": ["--flag"],
      "env": ["KEY=VALUE"]
    }
  }
}
```

支持的传输方式：

| type | 说明 | 必填字段 |
|---|---|---|
| `streamablehttp` | Streamable HTTP (推荐) | `url`，可选 `headers` |
| `sse` | Server-Sent Events | `url`，可选 `headers` |
| `stdio` | 标准输入输出子进程 | `command`，可选 `args`、`env` |

### 后台发布与日历来源

开启 `TOOLS_ENABLED` 并在 `MCP_CONFIG_PATH` 配置可以后台读取的全局 HTTP/SSE MCP 工具。为每个全局后台来源设置稳定、非空的 `backgroundIdentity`。同一来源重启后沿用原标识；更换来源时使用新标识并重新承接委托。下例中的来源标识、URL、工具名与仓库请替换为部署值。

```json
{
  "mcpServers": {
    "upstream": {
      "type": "streamablehttp",
      "url": "https://mcp.example.org/mcp",
      "backgroundIdentity": "upstream-releases-v1",
      "background": {
        "list_releases": {
          "operation": "release.list",
          "target": "owner/project",
          "arguments": {"repository": "owner/project"}
        }
      }
    }
  }
}
```

`release.list` 和 `event.list` 接受 MCP `structuredContent` 或一段 JSON 文本，形如 `{"events":[{"id":"v2.0.0","title":"Stable v2.0.0","summary":"Stable release includes fix #41","url":"https://example.org/releases/v2.0.0"}]}`。请用稳定的版本或活动 ID，返回目前仍可据此通知的条目。发布源应明确提供发布渠道和修复包含关系。活动可带 RFC3339 的 `start` 和 `end`。

个人日历创建需要本人在私聊导入 HTTP/SSE MCP，给对应工具设置 `"operation":"calendar.create"`、目标日历和固定的 `{"calendarId":"工作日历ID"}` 参数。工具还需接受活动的 `title`、`start`、`end`。只在本人明确委托、当时连接可用且只承接一次符合条件的活动时创建。成功回包应包含活动 `id` 和 `calendarId`。只有连接明确支持 `idempotencyKey` 的写入去重时才应设置 `idempotent: true`。群聊和兴趣关注不能以用户身份写入日历。接入新工具后，已有委托按原承接范围履行，需要扩约时请重新说明。

日历写入前会按原约核实来源，活动必须尚未开始。如果服务对重放请求返回 `not_applied`，较早发出但没有回执的写入仍会显示为待确认。只有已登记的幂等连接可以在活动开始前用原参数和原键重放。各次请求都已确认未写入时，Bot 会重新查源再决定是否创建。状态提示单独记录送达结果。确认创建后，Bot 只补发仍可重试的正式通知。

个人连接的导入、管理与群内分享见[个人 MCP 使用说明](usage.md#个人-mcp)。

### MCP 工具摘要（Lazy Loading，可选）

默认情况下，每次 LLM 请求都会把所有注册工具的完整 schema 随 `Tools` 字段发出，工具数量多时会消耗大量 token。启用工具摘要后，流程变为两阶段：

1. **Phase 1（始终注入，极小体积）**：系统提示词中附加每个 MCP server 的一段功能摘要；`Tools` 字段仅包含虚拟工具 `get_tool_details`。
2. **Phase 2（按需加载）**：LLM 判断需要某个工具时，先调用 `get_tool_details({"groups":["server_name"]})`；bot 将该 server 的完整 schema 注入后续请求，LLM 再发出真实工具调用。

摘要由可配置的小模型在 MCP server 首次连接时自动生成，持久化保存到 `user_mcp.db`；工具列表变化（SHA-256 哈希不匹配）时自动重新生成。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `TOOLS_SUMMARY_ENABLED` | `false` | 是否启用两阶段 Lazy Loading |
| `TOOLS_SUMMARY_API_BASE` | `OPENAI_API_BASE` | 摘要生成模型的 API 地址（留空回退主配置） |
| `TOOLS_SUMMARY_API_KEY` | `OPENAI_API_KEY` | 摘要生成模型的 API Key（留空回退主配置） |
| `TOOLS_SUMMARY_MODEL` | `OPENAI_MODEL` | 摘要生成模型名称（推荐使用轻量/廉价模型） |

说明：

- `TOOLS_SUMMARY_ENABLED=false`（默认）时，行为与原来完全一致，不影响任何现有工具调用逻辑
- 全局 MCP server 摘要在 bot 启动时后台预生成；用户动态添加 MCP 后异步生成，完成前 `/mcp_list` 和系统提示中以 `**server_name** (N tools)` 占位
- 删除 MCP server（`/mcp_del` / `/mcp_clear`）时对应摘要缓存同步清除
- 可通过 `/admin` 热修改所有 `TOOLS_SUMMARY_*` 配置项，无需重启

## 自然语言提醒

| 环境变量 | 说明 |
|---|---|
| `REMINDER_DEFAULT_TIMEZONE` | 解析器使用的默认 IANA 时区（默认 `Asia/Shanghai`） |
| `REMINDER_API_BASE` | 可选：解析器专用 API base，为空则回退主 OpenAI 客户端 |
| `REMINDER_API_KEY` | 可选：解析器专用 API key |
| `REMINDER_MODEL` | 可选：解析器专用 model，为空则回退 `OPENAI_MODEL` |

所有配置均已接入 `/admin` 热修改面板，`REMINDER_DEFAULT_TIMEZONE` 设置时会验证 IANA 时区有效性。

## 每日用量配额

bot 复用现有 `internal/app/stats.go` 的 usage 管线，在其上层叠加一层内存配额计数器
（`quotaCounter`），按 `QUOTA_RESET_TZ` 指定的时区按天滚动。计数器在进程启动时
从 `chat.db` 的 `usage_daily` bucket 读取今日记录回填，避免重启后放空已封顶的
用户。所有配置项均支持 `/admin` 热修改，无需重启。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `USER_DAILY_TOKEN_LIMIT` | `0` | 每用户每日 token 上限；仅累计 `main_chat` + `tool_round` 两种 call type 的 token。`0` 表示不限制 |
| `USER_DAILY_USD_LIMIT` | `0` | 每用户每日 USD 上限；同样仅统计 `main_chat` + `tool_round`，`0` 表示不限制 |
| `CHAT_DAILY_TOKEN_LIMIT` | `0` | 每聊天每日 token 上限；群聊可避免单个 chat 被刷爆，`0` 表示不限制 |
| `MODEL_PRICE_TABLE` | `""` | 模型价目表，格式 `"gpt-4o:0.0025/0.01,gpt-4o-mini:0.00015/0.0006"`；key 为小写 model 名，值为 input / output 每 1k token 的 USD 单价。未列入表的模型成本按 0 计算（首次遇到时告警一次） |
| `QUOTA_RESET_TZ` | `Asia/Shanghai` | 每日配额滚动所用的 IANA 时区；无效时回落到 UTC |

行为细节：

- 配额判断只看 `main_chat` + `tool_round`（即用户可见的对话轮次）。`summary` / `profile_extract` / `relevance_check` / `sticker_strategy` / `reminder_parse` 等后台调用**不**计入配额上限，但**仍**写入 Dashboard 的 USD 总额（你支付的就该被看见）
- `ADMIN_ID` 明确列出的管理员可绕过配额检查。生效名单中有一项为 `*` 时，所有用户都适用配额限制。
- 超额后的处理：handleText / handleVoice / handlePhoto / handleDocument 四个入口在 `isAllowed` 之后、真正发起下载或 LLM 调用之前立即短路，返回中文友好提示（`💳 本日个人 token 配额已用完…`），**不**触发 LLM 请求、**不**扣链路消耗。正在进行中的流式回复不会被中断
- Dashboard 的 **Today USD / Month USD** 两张卡按 `QUOTA_RESET_TZ` 计算起止时间，`SumDailyRange` 仅扫描 `usage_daily` 下 `scope=chat` 的聚合避免跨 scope 重复计入
- **历史 USD 不回填**：部署前写入的 `usage_daily` 行没有 `cost_usd` 字段，反序列化时视为 0。"Month USD" 卡片标注 *since deploy* 以避免因价格随时间变化导致的重算失真
- `/quota_reset` 命令会同时清空内存 counter **并**删除今日的 `usage_daily` 行，确保重启后 hydrate 不会把旧值读回来

管理员操作见[重置每日配额](usage.md#重置每日配额)。
