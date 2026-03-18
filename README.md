# LLM Telegram Bot

> ⚠️本项目完全采用 **Vibe Coding** 方式开发。

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
- **可选 TTS 语音播报** — 管理员可按聊天开启 `/speach` 模式，机器人会自动缩短回复并额外发送火山引擎合成音频
- **贴纸策略发送** — 支持在回复后按规则/模型策略自动发送 Telegram Sticker，支持 `append` / `replace` / `command_only` 模式
- **管理员热修改配置** — 管理员可在私聊中通过 `/admin` 查看、修改并持久化保存全部配置项，支持 `cancel` 返回上一步
- **并发安全** — 快照 + 原子追加机制，多个并发请求不会导致上下文错乱
- **白名单 / 权限控制** — 通过 `ALLOWED_USERS` 和 `ALLOWED_GROUPS` 限制 bot 的使用范围
- **OpenAI 兼容** — 支持任何 OpenAI 兼容 API（如 DeepSeek、通义千问、Ollama 等）
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

# 复制并编辑 YAML 主配置
cp config.yaml.example config.yaml
# 复制贴纸规则示例（如需贴纸能力）
cp sticker_rules.json.example sticker_rules.json

# 复制并编辑环境变量覆盖（建议只放密钥）
cp .env.example .env
# 编辑 config.yaml / sticker_rules.json / .env

# 运行
go run .
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
docker run --env-file .env -v "$(pwd)/data:/app/data" llm-telebot
```

### 直接拉取镜像运行

每次 push 会自动构建并发布镜像到 GitHub Container Registry：

```bash
mkdir -p data
cp config.yaml.example ./data/config.yaml
cp .env.example .env

podman pull ghcr.io/lxbme/llm_telebot:latest
podman run --env-file .env -v "$(pwd)/data:/app/data" ghcr.io/lxbme/llm_telebot:latest
```


## 配置说明

默认使用 `config.yaml` 作为主配置文件，环境变量（包括 `.env` 加载进来的变量）会覆盖同名项。

配置优先级：

```text
config.yaml -> 环境变量覆盖
```

推荐做法：

- 将大部分业务配置写在 `config.yaml`
- 将 API Key、Token、容器路径覆盖等写在 `.env`
- 如果某个键同时出现在 `config.yaml` 和环境变量中，启动时会以环境变量为准
- `/admin` 会把修改写回 `config.yaml`；若该项仍被环境变量覆盖，则重启后会恢复为环境变量的值

### 配置文件位置

- 本地直接运行时，默认配置文件路径为 `./config.yaml`
- 容器镜像内默认配置文件路径为 `/app/data/config.yaml`
- 可通过环境变量 `CONFIG_FILE` 自定义配置文件路径

### `/admin` 热修改说明

- 仅管理员私聊可用
- 发送 `/admin` 后，bot 会列出全部配置项及编号
- 回复编号后进入编辑态，再回复新值即可立即生效并写回 `config.yaml`
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
| `CONFIG_FILE` | 否 | `./config.yaml` | YAML 主配置文件路径；容器镜像中默认是 `/app/data/config.yaml` |
| `BOT_USERNAME` | 否 | 自动获取 | 机器人用户名（带 @ 前缀），用于群聊中检测 @提及 |
| `SYSTEM_PROMPT` | 否 | `You are a helpful assistant.` | 系统提示词 |
| `CONTEXT_MAX_MESSAGES` | 否 | `20` | 每个对话保留的最大消息数（滑动窗口） |
| `MAX_TOKENS` | 否 | `0` | 每次回复的最大 token 数，0 表示不限 |

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

### Telegram Sticker 策略（可选）

贴纸能力采用“回复后处理”设计：先完成文本/语音主流程，再决定是否发送贴纸。策略优先使用关键词规则，规则未命中时可选模型辅助选标签。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `STICKER_ENABLED` | `false` | 是否启用贴纸策略 |
| `STICKER_MODE` | `append` | 贴纸模式：`off` / `append` / `replace` / `command_only` |
| `STICKER_PACK_NAME` | `""` | 贴纸包业务别名（预留字段） |
| `STICKER_RULES_PATH` | `./sticker_rules.json` | 贴纸规则文件路径（JSON） |
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

默认使用 `sticker_rules.json`（容器中通常建议放在 `./data/sticker_rules.json`，并在 `STICKER_RULES_PATH` 指向该路径）。

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

**扩展内置工具**：实现 `MCPTool` 接口（`Name`/`Description`/`Parameters`/`Execute`），在 `builtin_tools.go` 的 `RegisterBuiltinTools()` 中注册即可。

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

## 架构简述

```
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
