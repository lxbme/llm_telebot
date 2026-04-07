# LLM Telegram Bot

一个基于 Go 开发的 Telegram 聊天机器人，接入 OpenAI 兼容 API，支持流式回复、多轮上下文、群聊智能检测、MCP 工具调用等功能。

## 功能特性

- **流式回复** — 实时编辑消息，逐步展示 LLM 的生成内容，无需等待完整回复
- **多轮对话上下文** — 滑动窗口记忆最近 N 条消息，支持连续对话
- **动态对话摘要** — 当对话超出滑动窗口时，自动将更早的对话压缩为摘要，赋予 LLM 更长的记忆能力
- **用户画像提取** — 从对话中自动提取用户兴趣、职业等标签，注入系统提示词让 LLM 了解对话对象
- **群聊支持** — 通过 @机器人 触发回复，自动识别并剥离 @mention
- **上下文模式**
  - `at` 模式：仅记录 @机器人 的消息作为上下文
  - `global` 模式：记录群聊中所有消息作为上下文，提供更完整的对话理解
- **智能自动检测 (AUTO_DETECT)** — 利用 LLM 判断群聊中未 @机器人 的消息是否与机器人相关，自动触发回复
- **独立模型配置** — AUTO_DETECT、用户画像提取、对话摘要均可配置独立的轻量模型，节省主模型 token 开销
- **工具调用 (MCP)** — LLM 可自主判断是否需要调用工具（如获取时间、计算器），并根据工具返回结果生成回复；支持通过实现 `MCPTool` 接口轻松扩展新工具
- **远程 MCP 服务器** — 通过 JSON 配置文件接入远程 MCP 服务器，支持 Streamable HTTP、SSE、Stdio 三种传输方式
- **动态 per-user MCP** — 每个用户可在聊天中通过发送 JSON 导入自己专属的 MCP 服务器，持久化存储、重启不丢失，通过命令管理
- **MCP 工具摘要（Lazy Loading）** — 启用后，仅将各 MCP server 的一段功能摘要注入上下文，LLM 在需要工具时再按需请求完整 schema，可显著降低每次请求的 token 开销；摘要由小模型自动生成并持久化，工具列表变化时自动刷新
- **聊天级定时任务** — 每个聊天都可以配置独立的定时任务，按 cron 表达式定时触发，并复用正常 chat 流程向 LLM 发送 prompt
- **SSH TUI 管理看板** — 可通过 SSH 登录内嵌 Dashboard，基于 [Bubble Tea](https://github.com/charmbracelet/bubbletea) 呈现多 Tab 运行态、用户画像、任务和详细事件流
- **语音输入 (STT)** — 接收 Telegram 语音消息，通过 OpenAI 兼容 STT API（Whisper、gpt-4o-transcribe 等）转写为文字后，走与文字消息完全相同的对话流程（历史记忆、工具调用、TTS 输出等）；可选是否向用户回显转写结果
- **可选 TTS 语音播报** — 管理员可按聊天开启 `/speach` 模式，机器人会自动缩短回复并额外发送火山引擎合成音频
- **贴纸策略发送** — 支持在回复后按规则/模型策略自动发送 Telegram Sticker，支持 `append` / `replace` / `command_only` 模式
- **管理员热修改配置** — 管理员可在私聊中通过 `/admin` 查看、修改并持久化保存全部配置项，支持 `cancel` 返回上一步
- **并发安全** — 快照 + 原子追加机制，多个并发请求不会导致上下文错乱
- **白名单 / 权限控制** — 通过 `ALLOWED_USERS` 和 `ALLOWED_GROUPS` 限制 bot 的使用范围
- **OpenAI 兼容** — 支持任何 OpenAI 兼容 API（如 DeepSeek、通义千问、Ollama 等）；自动适配新一代模型（o1/o3/o4、gpt-5+）的 API 限制：自动切换 `max_completion_tokens` 字段，并自动清除不受支持的采样参数（`temperature`、`top_p` 等）
- **用户身份追踪** — 每条消息自动附带发送者信息，LLM 能区分不同用户

## 快速开始

### 前置要求

- Go 1.21+
- 一个 Telegram Bot Token（从 [@BotFather](https://t.me/BotFather) 获取）
- 一个 OpenAI 兼容 API Key

### 安装与运行

```bash
# 克隆项目
git clone https://github.com/lxbme/llm_telebot.git
cd llm_telebot

# 统一使用 data 目录存放可持久化配置
mkdir -p data
cp config.yaml.example ./data/config.yaml
cp sticker_rules.json.example ./data/sticker_rules.json

# 复制并编辑环境变量覆盖（建议只放密钥）
cp .env.example .env
# 编辑 ./data/config.yaml / ./data/sticker_rules.json / .env

# 如需 SSH Dashboard，先准备 host key 与 authorized_keys
ssh-keygen -t ed25519 -f ./data/dashboard_ssh_ed25519 -N ""
# 将你自己的 SSH 公钥追加到 ./data/dashboard_authorized_keys
# 然后在 ./data/config.yaml 中设置 DASHBOARD_SSH_ENABLED: true

# 运行
CONFIG_FILE=./data/config.yaml go run ./cmd/llm_telebot
```

### 使用预构建镜像部署（推荐）

```bash
git clone https://github.com/lxbme/llm_telebot.git

# 准备数据目录
mkdir -p data
cp config.yaml.example ./data/config.yaml
cp sticker_rules.json.example ./data/sticker_rules.json
cp .env.example .env
# 编辑 ./data/config.yaml / ./data/sticker_rules.json 与 .env

# 如需 MCP 工具，编辑 ./mcp_config.json（可选）
# 如需 SSH Dashboard，请确认 ./data/config.yaml 中已启用 DASHBOARD_SSH_ENABLED
# compose 文件默认已映射 23234:23234

# 使用预构建镜像一键启动
podman compose up -f ./docker-compose.deploy.yml -d

# 查看日志
podman logs -f llm-telebot

# 拉取最新镜像并重启
podman compose pull && podman compose up -f ./docker-compose.deploy.yml -d
```


### 本地 Docker Compose 构建运行

如需在本地自行构建镜像，可继续使用项目根目录下的 `docker-compose.yml`：

```bash
mkdir -p data
cp config.yaml.example ./data/config.yaml
cp sticker_rules.json.example ./data/sticker_rules.json
cp .env.example .env

# compose 文件默认已映射 23234:23234，启用 Dashboard 后可直接从宿主机 SSH 登录
docker compose up -d
docker compose logs -f
docker compose down
```

### 手动 Docker 构建

```bash
mkdir -p data
cp config.yaml.example ./data/config.yaml
cp sticker_rules.json.example ./data/sticker_rules.json
cp .env.example .env

docker build -t llm-telebot .
docker run --env-file .env -p 23234:23234 -v "$(pwd)/data:/app/data" llm-telebot
```

### 直接拉取镜像运行

每次 push 会自动构建并发布镜像到 GitHub Container Registry：

```bash
mkdir -p data
cp config.yaml.example ./data/config.yaml
cp sticker_rules.json.example ./data/sticker_rules.json
cp .env.example .env

podman pull ghcr.io/lxbme/llm_telebot:latest
podman run --env-file .env -p 23234:23234 -v "$(pwd)/data:/app/data" ghcr.io/lxbme/llm_telebot:latest
```


## SSH Dashboard

项目内置一个只读 SSH TUI 看板，用于查看全局概览、按 `userID` 聚合的用户详情、按聊天维度的定时任务，以及详细事件流。

### Dashboard 能看到什么

- `Overview`：最近窗口请求数、成功率、总 token、热点聊天、最近活跃用户/聊天
- `Users`：左侧用户列表，右侧展示 `userID`、`username`、`displayName`、profile、个人 MCP、个人 usage 汇总
- `Schedules`：按聊天查看定时任务、下次执行时间、上次执行时间、最近错误
- `Logs`：比进程终端更细的结构化事件流，包括对话、usage、tool、MCP、schedule、profile、config 等事件

### 启用步骤

1. 生成服务端 host key：

```bash
ssh-keygen -t ed25519 -f ./data/dashboard_ssh_ed25519 -N ""
```

2. 创建 `authorized_keys` 白名单：

```bash
mkdir -p ./data
touch ./data/dashboard_authorized_keys
chmod 600 ./data/dashboard_authorized_keys
```

3. 把允许登录的客户端公钥写进去，一行一个，例如：

```text
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... your-name@machine
```

4. 在 `./data/config.yaml` 中开启：

```yaml
DASHBOARD_SSH_ENABLED: true
DASHBOARD_SSH_LISTEN: :23234
DASHBOARD_SSH_HOST_KEY_PATH: ./data/dashboard_ssh_ed25519
DASHBOARD_SSH_AUTHORIZED_KEYS_PATH: ./data/dashboard_authorized_keys
```

5. 重启 bot 进程或容器。

### 本地连接示例

```bash
ssh -i ~/.ssh/id_ed25519 -p 23234 dashboard@127.0.0.1
```

说明：

- `-i` 指向的是与你写入 `authorized_keys` 的那条公钥相对应的私钥
- 用户名只是 SSH 会话字段，占位即可，例如 `dashboard`
- 如果通过 Docker/Podman 运行，需要 compose 暴露 `23234:23234`，项目中的两个 compose 文件已包含该映射

### TUI 快捷键

- `Tab` / `Shift+Tab`：切换 Tab
- `h` / `l`：切换 Tab
- `j` / `k`：在列表中移动
- `r`：手动刷新
- `q`：退出

## 配置说明

本文档统一推荐使用 `./data/config.yaml` 作为主配置文件，环境变量（包括 `.env` 加载进来的变量）会覆盖同名项。

配置优先级：

```text
./data/config.yaml -> 环境变量覆盖
```

推荐做法：

- 将大部分业务配置写在 `./data/config.yaml`
- 将 API Key、Token、容器路径覆盖等写在 `.env`
- 如果某个键同时出现在 `./data/config.yaml` 和环境变量中，启动时会以环境变量为准
- `/admin` 会把修改写回当前 `CONFIG_FILE` 指向的文件；若该项仍被环境变量覆盖，则重启后会恢复为环境变量的值

### 配置文件位置

- 本地直接运行时，若未设置 `CONFIG_FILE`，默认配置文件路径为 `./data/config.yaml`（代码常量：`DefaultConfigFilePath`，定义于 `internal/app/config_file.go`）
- 容器镜像内默认配置文件路径为 `/app/data/config.yaml`
- 推荐通过环境变量 `CONFIG_FILE=./data/config.yaml` 统一本地与容器路径

### `/admin` 热修改说明

- 仅管理员私聊可用
- 发送 `/admin` 后，bot 会列出全部配置项及编号
- 回复编号后进入编辑态，再回复新值即可立即生效并写回 `CONFIG_FILE` 指向的配置文件
- 两步消息都带 `cancel` 按钮，可返回上一步或退出
- 输入 `<empty>` 可清空字符串或列表配置
- `TELEGRAM_BOT_TOKEN` 修改后会写入运行时与配置文件，但建议重启进程后再确认轮询连接状态

### 核心配置

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
| `MAX_TOKENS` | 否 | `0` | 每次回复的最大 token 数，0 表示不限。新一代模型（o1/o3/o4 系列及 gpt-5+）会自动切换为 `max_completion_tokens` 字段，并自动清除 `temperature`、`top_p` 等不受支持的采样参数，无需额外配置 |

### SSH Dashboard（可选）

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `DASHBOARD_SSH_ENABLED` | `false` | 是否启用内嵌 SSH TUI 看板 |
| `DASHBOARD_SSH_LISTEN` | `:23234` | Dashboard SSH 监听地址 |
| `DASHBOARD_SSH_HOST_KEY_PATH` | `./data/dashboard_ssh_ed25519` | SSH 服务端 host key 私钥路径 |
| `DASHBOARD_SSH_AUTHORIZED_KEYS_PATH` | `./data/dashboard_authorized_keys` | 允许登录的客户端公钥白名单文件 |
| `DASHBOARD_SSH_IDLE_TIMEOUT` | `1h` | 单个 SSH session 的空闲超时 |
| `DASHBOARD_SSH_MAX_SESSIONS` | `8` | 同时允许的最大 Dashboard 会话数 |

说明：

- `HOST_KEY` 是服务器证明自身身份的私钥文件，应该保密
- `AUTHORIZED_KEYS` 是客户端公钥白名单，格式与 OpenSSH 的 `authorized_keys` 一致
- 客户端连接时仍需要本地持有与公钥匹配的私钥，服务器不会保存你的私钥

### 火山引擎 TTS（可选）

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

### 语音输入 STT（可选）

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

### Telegram Sticker 策略（可选）

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

### 贴纸规则文件格式

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

### 群聊行为配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `CONTEXT_MODE` | `at` | `at` = 仅 @消息作为上下文；`global` = 所有群聊消息作为上下文 |
| `AUTO_DETECT` | `false` | `true` = 自动判断未 @消息 是否与机器人相关并回复 |

### 访问控制 / 白名单（可选）

当 `ALLOWED_USERS` 和 `ALLOWED_GROUPS` 均未设置时，bot 对所有人开放。设置后，只有白名单中的用户/群组可以使用 bot。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `ALLOWED_USERS` | — | 允许使用 bot 的用户 ID 列表，逗号分隔。适用于私聊；群聊中该用户也被放行 |
| `ALLOWED_GROUPS` | — | 允许使用 bot 的群组/超级群组 ID 列表，逗号分隔。群内所有成员均可使用 |

> **提示：** 获取用户/群组 ID 的方法：向 [@userinfobot](https://t.me/userinfobot) 发送消息可获取用户 ID；将 [@RawDataBot](https://t.me/RawDataBot) 加入群组可获取群组 ID（通常为负数）。
>
> 示例：`ALLOWED_USERS=123456789,987654321`  `ALLOWED_GROUPS=-1001234567890`

### 管理员配置（可选）

管理员可以通过 JSON 导入 command-based (stdio) MCP 服务器。非管理员用户只能添加 HTTP/SSE 类型的远程 MCP 服务器。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `ADMIN_ID` | — | 管理员用户 ID 列表，逗号分隔。设为 `*` 表示所有用户均为管理员 |

> **容器化场景：** 当 bot 运行在 Docker 容器中时，stdio MCP 子进程也在容器内执行，天然具有沙箱隔离。此时可安全地设置 `ADMIN_ID=*` 允许所有用户添加 command-based MCP 服务器。

### 自动检测独立模型（可选）

当 `AUTO_DETECT=true` 时，可为相关性判断配置独立的轻量模型，节省主模型的 token 消耗。未设置时，回退到主模型配置。

| 环境变量 | 回退值 | 说明 |
|---|---|---|
| `AUTO_DETECT_API_BASE` | `OPENAI_API_BASE` | 检测模型的 API 地址 |
| `AUTO_DETECT_API_KEY` | `OPENAI_API_KEY` | 检测模型的 API Key |
| `AUTO_DETECT_MODEL` | `OPENAI_MODEL` | 检测模型名称（推荐使用 `gpt-4o-mini` 等轻量模型） |

### 用户画像提取（可选）

自动从对话中提取用户的兴趣、职业、位置等标签，存入本地 bbolt 数据库。提取结果会注入到系统提示词中，帮助 LLM 了解对话对象。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `PROFILE_ENABLED` | `true` | 是否启用用户画像提取 |
| `PROFILE_DB_PATH` | `./data/profiles.db` | bbolt 数据库文件路径 |
| `PROFILE_EXTRACT_EVERY` | `3` | 每 N 次 bot 回复触发一次后台提取（另有 2 分钟冷却） |
| `PROFILE_API_BASE` | `OPENAI_API_BASE` | 画像提取模型的 API 地址 |
| `PROFILE_API_KEY` | `OPENAI_API_KEY` | 画像提取模型的 API Key |
| `PROFILE_MODEL` | `OPENAI_MODEL` | 画像提取模型名称（推荐轻量模型） |

### 对话摘要（自动长期记忆）

当滑动窗口溢出时，溢出的消息不会丢失，而是被后台 LLM 调用压缩成一段摘要。每次发给 LLM 的上下文结构为：

```
[系统提示词 + 用户画像 + 历史摘要] + [滑窗内原始对话] + [当前用户消息]
```

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `SUMMARY_ENABLED` | `true` | 是否启用对话摘要 |
| `SUMMARY_MIN_OVERFLOW` | `6` | 累积多少条溢出消息后才触发一次摘要（避免频繁调用） |
| `SUMMARY_API_BASE` | `OPENAI_API_BASE` | 摘要模型的 API 地址 |
| `SUMMARY_API_KEY` | `OPENAI_API_KEY` | 摘要模型的 API Key |
| `SUMMARY_MODEL` | `OPENAI_MODEL` | 摘要模型名称（推荐轻量模型） |

### 工具调用 / MCP

启用后，LLM 可自主判断是否需要调用注册的工具，并根据工具返回结果生成最终回复。

#### 全局 MCP 配置

通过 JSON 配置文件接入远程 MCP 服务器，所有用户共享这些工具。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `TOOLS_ENABLED` | `false` | 是否启用工具调用 |
| `TOOLS_MAX_ITERATIONS` | `5` | 每次请求最多允许的工具调用轮数（防止无限循环） |
| `MCP_CONFIG_PATH` | — | 全局 MCP 服务器 JSON 配置文件路径 |
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

#### 动态 Per-User MCP

每个用户可以在聊天中导入自己专属的 MCP 服务器。**用户的 MCP 工具只对该用户可见**，不同用户互不干扰。配置持久化存储在 bbolt 中，重启后自动恢复连接。

**导入方式**：直接向 bot 发送一条 JSON 消息即可：

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

**管理命令**：

| 命令 | 说明 |
|---|---|
| `/mcp_list` | 查看你的个人 MCP 服务器列表 |
| `/mcp_del <name>` | 删除指定名称的 MCP 服务器 |
| `/mcp_clear` | 清除你所有的个人 MCP 服务器 |

管理命令说明：`/mcp_list` 在摘要生成完成后会在每条服务器信息下方展示其功能摘要，便于快速了解该 server 的用途。

**扩展内置工具**：实现 `MCPTool` 接口（`Name`/`Description`/`Parameters`/`Execute`），在 `builtin_tools.go` 的 `RegisterBuiltinTools()` 中注册即可。

#### MCP 工具摘要（Lazy Loading，可选）

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

### 定时任务 / Schedule

定时任务按 `chat_id` 隔离保存：不同私聊、群聊之间互不共享。任务触发时，bot 会自动向 LLM 发送一条预设 `prompt`，然后走和普通聊天相同的回复链路，把结果发回当前聊天。

权限规则：

- `upsert`、`delete`、`pause`、`resume` 仅管理员可用
- `list`、`help`、`example` 可在当前聊天直接查看

#### 创建或更新任务

发送一条 JSON 消息即可：

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

#### Schedule 命令

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

#### 常用示例

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

## Bot 命令

| 命令 | 说明 |
|---|---|
| `/start` | 显示欢迎信息 |
| `/clear` | 清除当前对话的上下文历史和摘要 |
| `/summary` | 查看当前对话的摘要内容 |
| `/speach [on\|off\|toggle\|status]` | 管理员切换当前聊天的 TTS 模式 |
| `/displayp` | 查看自己的用户画像 |
| `/clearp` | 清除自己的用户画像 |
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

## BotFather 命令列表

下面这份可以直接发给 BotFather 的 `/setcommands`：

```text
start - 显示欢迎信息
clear - 清除当前对话上下文和摘要
summary - 查看当前对话摘要
speech - 切换当前聊天的语音模式
sticker - 查看贴纸状态或发送贴纸
displayp - 查看自己的用户画像
clearp - 清除自己的用户画像
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
admin - 管理员配置面板
```

说明：

- `speech` 和代码里的 `/speach` / `/speech` 都可用，推荐在 BotFather 中只展示 `speech`
- BotFather 的命令描述建议尽量简短，否则容易超长

## 架构简述

```
用户语音 → handleVoice()
            ├─ 权限检查: isAllowed()
            ├─ STT 未配置 → 静默忽略
            ├─ 下载 OGG/OPUS 流 → STT API (/v1/audio/transcriptions)
            ├─ STT_DISPLAY=true → 回复 🎙️ <转写内容>
            └─ 以转写文本构建 userMsg → startChatFlow() （与文字消息完全相同的后续流程）

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
  1. 快照当前上下文 (snapshot)
  2. 注入用户画像 + 对话摘要到系统提示词
  3. 构建 [system_prompt + 摘要 + 画像 + snapshot + user_msg] 发送给 LLM
  4. 合并工具视图: 全局工具 + 用户个人 MCP 工具 → MergedToolView
  5. 若启用工具调用 → 非流式请求，LLM 可能返回 tool_calls
     ├─ 有 tool_calls → 执行工具（全局或用户专属），将结果追加到消息，循环回步骤 5
     └─ 无 tool_calls → 进入流式路径
  6. 流式接收 LLM 最终回复，每 1.5s 更新 Telegram 消息
  7. 完成后原子追加 [user_msg, assistant_reply] 到历史
  8. 若溢出消息 ≥ SUMMARY_MIN_OVERFLOW → 后台触发摘要压缩
  9. 若满足提取条件 → 后台触发用户画像提取
 10. 执行 `finalizeSpeechReply()` 与 `finalizeStickerReply()` 收尾（按配置发送语音/贴纸）
```

## 技术依赖

- [telebot v3](https://github.com/tucnak/telebot) — Telegram Bot 框架
- [go-openai](https://github.com/sashabaranov/go-openai) — OpenAI Go SDK
- [godotenv](https://github.com/joho/godotenv) — .env 文件加载
- [bbolt](https://github.com/etcd-io/bbolt) — 嵌入式 key-value 数据库（用户画像 + MCP 配置持久化）
- [mcp-go](https://github.com/mark3labs/mcp-go) — MCP (Model Context Protocol) Go 客户端

## License

MIT
