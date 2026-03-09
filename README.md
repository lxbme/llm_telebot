# LLM Telegram Bot

一个基于 Go 开发的 Telegram 聊天机器人，接入 OpenAI 兼容 API，支持流式回复、多轮上下文、群聊智能检测等功能。

## 功能特性

- **流式回复** — 实时编辑消息，逐步展示 LLM 的生成内容，无需等待完整回复
- **多轮对话上下文** — 滑动窗口记忆最近 N 条消息，支持连续对话
- **群聊支持** — 通过 @机器人 触发回复，自动识别并剥离 @mention
- **上下文模式**
  - `at` 模式：仅记录 @机器人 的消息作为上下文
  - `global` 模式：记录群聊中所有消息作为上下文，提供更完整的对话理解
- **智能自动检测 (AUTO_DETECT)** — 利用 LLM 判断群聊中未 @机器人 的消息是否与机器人相关，自动触发回复
- **独立检测模型** — AUTO_DETECT 可配置单独的轻量模型（如 gpt-4o-mini），节省 token 开销
- **并发安全** — 快照 + 原子追加机制，多个并发请求不会导致上下文错乱
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
git clone <your-repo-url>
cd llm_telebot

# 复制并编辑配置
cp .env.example .env
# 编辑 .env 填入你的 API Key 和 Bot Token

# 运行
go run .
```

### 使用 Docker（可选）

```bash
docker build -t llm-telebot .
docker run --env-file .env llm-telebot
```

## 配置说明

所有配置通过环境变量或 `.env` 文件设置。

### 核心配置

| 环境变量 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `OPENAI_API_BASE` | 否 | `https://api.openai.com/v1` | OpenAI 兼容 API 的 Base URL |
| `OPENAI_API_KEY` | **是** | — | API 密钥 |
| `OPENAI_MODEL` | 否 | `gpt-4o` | 模型名称 |
| `TELEGRAM_BOT_TOKEN` | **是** | — | Telegram Bot Token |
| `BOT_USERNAME` | 否 | 自动获取 | 机器人用户名（带 @ 前缀），用于群聊中检测 @提及 |
| `SYSTEM_PROMPT` | 否 | `You are a helpful assistant.` | 系统提示词 |
| `CONTEXT_MAX_MESSAGES` | 否 | `20` | 每个对话保留的最大消息数（滑动窗口） |
| `MAX_TOKENS` | 否 | `0` | 每次回复的最大 token 数，0 表示不限 |

### 群聊行为配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `CONTEXT_MODE` | `at` | `at` = 仅 @消息作为上下文；`global` = 所有群聊消息作为上下文 |
| `AUTO_DETECT` | `false` | `true` = 自动判断未 @消息 是否与机器人相关并回复 |

### 自动检测独立模型（可选）

当 `AUTO_DETECT=true` 时，可为相关性判断配置独立的轻量模型，节省主模型的 token 消耗。未设置时，回退到主模型配置。

| 环境变量 | 回退值 | 说明 |
|---|---|---|
| `AUTO_DETECT_API_BASE` | `OPENAI_API_BASE` | 检测模型的 API 地址 |
| `AUTO_DETECT_API_KEY` | `OPENAI_API_KEY` | 检测模型的 API Key |
| `AUTO_DETECT_MODEL` | `OPENAI_MODEL` | 检测模型名称（推荐使用 `gpt-4o-mini` 等轻量模型） |

## Bot 命令

| 命令 | 说明 |
|---|---|
| `/start` | 显示欢迎信息 |
| `/clear` | 清除当前对话的上下文历史 |

## 架构简述

```
用户消息 → handleText()
            ├─ 私聊: 直接处理
            └─ 群聊: 检查 @mention
                     ├─ 被 @ → 处理
                     ├─ 未被 @ + AUTO_DETECT → isRelevant() 判断
                     │                         ├─ 相关 → 处理
                     │                         └─ 不相关 → 忽略
                     └─ 未被 @ → CONTEXT_MODE=global 时存入上下文，不回复

处理流程:
  1. 快照当前上下文 (snapshot)
  2. 构建 [system_prompt + snapshot + user_msg] 发送给 LLM
  3. 流式接收 → 每 1.5s 更新 Telegram 消息
  4. 完成后原子追加 [user_msg, assistant_reply] 到历史
```

## 技术依赖

- [telebot v3](https://github.com/tucnak/telebot) — Telegram Bot 框架
- [go-openai](https://github.com/sashabaranov/go-openai) — OpenAI Go SDK
- [godotenv](https://github.com/joho/godotenv) — .env 文件加载

## License

MIT
