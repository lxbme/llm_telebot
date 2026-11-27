# 部署与更新

你需要一个 Telegram Bot Token 和一个 OpenAI 兼容模型服务的 API Key。Telegram Bot Token 可通过 [@BotFather](https://t.me/BotFather) 获取。

## 准备配置

首次部署时，获取源码并复制配置模板。已有部署可沿用现有配置文件和数据目录。

```bash
git clone https://github.com/lxbme/llm_telebot.git
cd llm_telebot

mkdir -p data
cp -n config.yaml.example ./data/config.yaml
cp -n sticker_rules.json.example ./data/sticker_rules.json
cp -n .env.example .env
```

在 `.env` 中填写 `TELEGRAM_BOT_TOKEN` 和 `OPENAI_API_KEY`。使用其他兼容服务时，在 `data/config.yaml` 中设置 `OPENAI_API_BASE` 和 `OPENAI_MODEL`。如需限制 Bot 的使用范围，请设置用户或群组白名单。配置文件和环境变量的优先级见[配置参考](configuration.md#配置文件与优先级)。

两个 Compose 文件都会把根目录的 `mcp_config.json` 挂载为文件。暂时不用 MCP 时，可为首次部署创建一个空配置，已有文件会保留。

```bash
if [ ! -e mcp_config.json ]; then
  printf '%s\n' '{"mcpServers":{}}' > mcp_config.json
fi
```

## 使用预构建镜像

用 Docker Compose 启动、查看日志和更新。

```bash
docker compose -f docker-compose.deploy.yml up -d
docker compose -f docker-compose.deploy.yml logs -f

docker compose -f docker-compose.deploy.yml pull
docker compose -f docker-compose.deploy.yml up -d
```

使用 Podman 时，将命令中的 `docker compose` 换成 `podman compose`。

GitHub Actions 为推送的分支和符合条件的版本标签发布镜像。`:latest` 跟随默认分支。镜像支持 Linux amd64 和 arm64。

启动后打开 Bot 的 Telegram 私聊并发送 `/start`。使用方式见[使用指南](usage.md)。

## 从源码运行

原生运行需要 Go 1.25.1 或更新版本。准备配置后，在项目根目录运行 Bot。

```bash
go run ./cmd/llm_telebot
```

原生运行时，语音播报需要 `ffmpeg`。PDF 和部分 Office 文档的文本提取分别依赖 `pdftotext` 和 `pandoc`，按启用的功能安装即可。预构建镜像包含这些程序。详情见[配置参考](configuration.md)。

## 使用 Compose 本地构建

准备好配置和 `mcp_config.json` 后，使用仓库中的 `docker-compose.yml` 本地构建。

```bash
docker compose up -d --build
docker compose logs -f
docker compose down
```

## 直接运行容器

既可以拉取预构建镜像，也可以从当前源码构建。

```bash
docker pull ghcr.io/lxbme/llm_telebot:latest
docker run --env-file .env -v "$(pwd)/data:/app/data" ghcr.io/lxbme/llm_telebot:latest
```

```bash
docker build -t llm-telebot .
docker run --env-file .env -v "$(pwd)/data:/app/data" llm-telebot
```

若直接运行的容器需要读取全局 MCP 配置，请额外挂载文件并传入容器内的路径，例如 `-v "$(pwd)/mcp_config.json:/app/mcp_config.json:ro" -e MCP_CONFIG_PATH=/app/mcp_config.json`。若需要访问内置 SSH 看板，可以添加 `-p 23234:23234`。两个 Compose 文件已包含该端口映射。看板仍需按[启用步骤](usage.md#ssh-dashboard)配置。
