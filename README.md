# AGY ACP Gateway

将 Google Antigravity CLI（`agy acp`）封装为标准 OpenAI 协议网关，直接兼容 `/v1/chat/completions` 与 `/v1/responses`，并内置轻量化 Web 控制台用于网页完成 Google OAuth 登录与运行监控。

- **双标准接口**：支持 OpenAI **Chat Completions** 与 **Responses API**，完整覆盖流式 SSE、思考/推理内容、Token 与缓存用量统计、多模态图片输入。
- **客户端工具调用**：通过本地虚拟 MCP 桥实现原生 Function Calling；同时支持 Chat 与 Responses 的完整历史重放（含 `store: false` 无状态续聊、跨会话/跨重启恢复）。
- **纯净代理环境**：默认自动剥离 `agy` 原生 12KB 编程提示词与内置编码工具，拒绝所有未授权的内置工具调用。
- **Web 控制台与自动保活**：浏览器内两步完成 Google 登录/切号/登出，支持查看实时状态、Token 用量、一键复制模型 ID 与接入代码，并在空闲时自动静默续期 OAuth 凭据。
- **零外部依赖**：纯 Go 标准库实现（Linux，Go 1.23+），提供 Docker AIO 一键部署、轻量 slim 镜像及预编译二进制。

---

## 快速开始（推荐 Docker Compose）

仓库自带 All-in-One（AIO）镜像构建配置，会自动通过官方安装脚本内置 `agy`，适合绝大多数部署场景。

### 1. 配置环境变量

```bash
cp .env.example .env
```

编辑 `.env`，至少填写一个你自定义的网关密钥（访问控制台和 OpenAI API 时使用）：

```dotenv
GATEWAY_API_KEY=your-secret-gateway-key

# 如果容器访问 Google 需要代理，取消注释并填入容器可达的代理地址
# HTTPS_PROXY=http://proxy.example:3128
# HTTP_PROXY=http://proxy.example:3128
# ALL_PROXY=socks5://proxy.example:1080
```

> 容器内的 `127.0.0.1` 指向容器自身。如果代理运行在宿主机，请使用宿主机局域网 IP 或 Docker 网桥地址。

### 2. 构建并启动

```bash
docker compose up -d --build
```

默认仅监听宿主机 `127.0.0.1:8080`，登录态持久化在 Docker 命名卷 `agy-data`（挂载到容器 `/data`）中。

> 如果想先不连外网离线体验，可用内置 mock 启动同一套控制台和接口：
> ```bash
> docker compose run --rm --service-ports acp-gateway -listen 0.0.0.0:8080
> ```

### 3. 在网页完成 Google 登录

1. 浏览器打开 `http://127.0.0.1:8080/`，输入 `.env` 中设置的 `GATEWAY_API_KEY` 解锁控制台。
2. 点击 **「开始 Google 登录」** -> 打开官方授权页完成授权。
3. 将回调页面显示的**授权码**或**完整回调 URL** 粘贴回输入框并点击 **「完成登录」**。
4. 等待顶部状态变为 **「就绪」**，即可使用 OpenAI 客户端接入。

### 4. 客户端调用示例

**Python（OpenAI SDK）**

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8080/v1",
    api_key="your-secret-gateway-key",
)

# Responses API
resp = client.responses.create(
    model="MODEL_ID",  # 可在控制台点击模型标签直接复制
    input="你好，简单介绍一下你自己",
)
print(resp.output_text)

# Chat Completions API
chat = client.chat.completions.create(
    model="MODEL_ID",
    messages=[{"role": "user", "content": "你好"}],
)
print(chat.choices[0].message.content)
```

**cURL**

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer your-secret-gateway-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "MODEL_ID",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'
```

---

## 其他部署方式

### 预编译二进制 / 源码运行

从 [GitHub Releases](https://github.com/RC-CHN/agy-acp-gateway/releases) 下载对应架构（Linux `amd64` / `arm64`）的压缩包，或在本地使用 Go 1.23+ 直接运行：

```bash
# 连接本机已安装的真实 agy（使用本机登录态，或通过网页控制台登录）
GATEWAY_API_KEY=your-key ./agy-acp-gateway \
  -listen 127.0.0.1:8080 \
  -agy "$(command -v agy)" \
  -live

# 不传 -agy 则启动内置离线 Mock 模式，方便客户端联调
go run -buildvcs=false ./agy_acp_gateway
```

> 监听非回环地址（如 `0.0.0.0:8080`）时，网关会强制要求设置 `GATEWAY_API_KEY`（或 `-api-key`）。

### 轻量 slim 镜像 + 宿主机 `agy`

如果宿主机已经安装了兼容的 Linux `agy`，可以直接拉取发布的轻量镜像并挂载宿主机二进制和独立状态目录：

```bash
state="${XDG_DATA_HOME:-$HOME/.local/share}/agy-acp-gateway"
install -d -m 700 "$state"

docker run --rm --init --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:8080:8080 \
  -v "$(realpath "$(command -v agy)"):/usr/local/bin/agy:ro" \
  -v "$state:/data" \
  -e HOME=/data \
  -e GATEWAY_API_KEY \
  -e HTTPS_PROXY -e NO_PROXY \
  ghcr.io/rc-chn/agy-acp-gateway:v0.1.0 \
  -listen 0.0.0.0:8080 -agy /usr/local/bin/agy -live
```

也可以本地自行构建 slim 镜像：`docker build --target slim -t agy-acp-gateway:slim .`。

---

## 接口与能力说明

### 端点一览

| 端点 | 说明 |
| --- | --- |
| `POST /v1/chat/completions` | Chat Completions（JSON / SSE、思考增量 `reasoning_content`、Function Calling、Token 用量、`stream_options.include_usage`） |
| `POST /v1/responses` | Responses API（JSON / 语义 SSE、推理摘要、Function Calling、`previous_response_id` 续聊、`store: false` 无状态重放） |
| `GET /v1/models` | 返回当前可用的 ACP 模型列表及自定义别名 |
| `GET /` | 内置 Web 控制台（API Key 鉴权，仅存于当前标签页 `sessionStorage`） |
| `GET /health`、`GET /ready` | 存活与就绪探针（未登录或并发已满时 `/ready` 返回 `503`） |

### 使用须知

- **纯净代理与安全隔离**：未显式指定 `-agent` 时，网关会自动在临时工作区生成极简配置 `gateway-clean`，移除 `agy` 默认的 12KB 编程提示词和全部内置编码工具，仅放行客户端注册的工具调用（如需恢复 `agy` 原生默认代理可传 `-agent default`）。
- **工具调用与无状态续聊**：
  - 所有工具均在客户端本地执行，网关负责把模型的工具调用透传出来并等待回传结果。
  - **有状态模式**（Chat 多轮 / Responses `previous_response_id`）：优先命中原 ACP 会话的原生 MCP 回调继续执行；同一批次并行调用会合并返回，结果可乱序回传。
  - **无状态与重放兜底**（Responses `store: false` 或 Chat 跨重启/过期会话）：携带完整对话及工具历史即可由新会话自动重放继续，不会重复触发客户端工具。
- **图片输入**：支持 PNG、JPEG、WebP、GIF 的 base64 data URL，单次请求解码总大小上限 20 MiB；暂不支持远程图片 URL。
- **协议限制**：
  - 工具目前仅支持 `type: function`（不支持服务端托管工具）。
  - ACP 协议本身不支持的采样参数（如 `temperature`、`top_p`、`max_output_tokens`、`stop`）不会限制模型输出。
  - 无状态重放仅透传可见文本、推理摘要、工具调用与结果、图片，无法恢复上游隐藏推理签名或 KV 缓存。
- **错误响应与上游状态码还原**：
  - 所有生成接口都会返回 `X-Request-Id` 与结构化 JSON 错误（SSE 开流后的错误以错误帧 / `response.failed` 结束）。
  - Linux `amd64` 镜像默认在 agy 1.5.14 的模型错误分类器读取 HTTP 状态码，并用当前会话的终态记录核实归属。错误响应的 `error.upstream.observation_source` 为 `frida` 或 `trajectory_store`，原始状态码在 `provider_http_status`；有可信的重试提示时才返回 `Retry-After`。可用 `GATEWAY_STATUS_OBSERVER=store` 单独读取会话库，或用 `off` 关闭观测。

---

## 登录态持久化与自动保活

- **数据卷持久化**：Docker Compose 默认将凭据保存在命名卷 `agy-data`（容器内 `/data`）。重建容器会保留登录态，**注意 `docker compose down -v` 会删除卷并清空登录信息**。修改项目目录或项目名时，可在 `.env` 中设置 `AGY_DATA_VOLUME=<旧卷名>` 复用原有登录态。
- **空闲自动保活**：在 `managed` 模式下，网关空闲时默认每 `30m` 静默检查一次登录态（不触发模型生成，零 Token 消耗），自动刷新并持久化 OAuth Token。可通过 `GATEWAY_AUTH_KEEPALIVE_INTERVAL=0` 关闭。
- **宿主机只读凭据模式**：如果你希望完全由宿主机维护登录态、禁止通过网页登录/登出，可设置 `GATEWAY_CREDENTIAL_MODE=external`，并将凭据文件挂载到 `/data/.gemini/antigravity-cli/antigravity-oauth-token`（确保运行 UID `65532` 可读，父目录可写）。

---

## 常用配置

### 环境变量

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `GATEWAY_API_KEY` | 无（Compose 必填） | 访问 API 与 Web 控制台的 Bearer 密钥 |
| `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` / `NO_PROXY` | 空 | 代理设置（Compose 会同时用于构建和运行阶段，支持小写） |
| `GATEWAY_DASHBOARD` | `true` | 是否启用内置 Web 控制台和管理接口 |
| `GATEWAY_CREDENTIAL_MODE` | `managed` | `managed` 允许网页登录/切号/登出；`external` 禁用网页认证写操作 |
| `GATEWAY_AUTH_KEEPALIVE_INTERVAL` | `30m` | 已登录状态下的空闲静默保活间隔，`0` 表示关闭 |
| `GATEWAY_STATUS_OBSERVER` | `frida`（amd64 镜像） | 上游状态码观测模式：`frida` / `store` / `off` |
| `AGY_DATA_VOLUME` | `<项目名>_agy-data` | 指定复用的 Docker 数据卷名称 |
| `AGY_EXPECTED_VERSION` / `AGY_EXPECTED_SHA256` | 空 | 可选构建参数，用于构建 AIO 镜像时校验 `agy` 版本与 ELF SHA-256 |

### 命令行参数

完整列表可执行 `./agy-acp-gateway -h` 查看，常用参数如下：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | HTTP 监听地址 |
| `-agy` | 空（内置 mock） | 真实 `agy` 可执行文件路径 |
| `-live` | `false` | 启用真实上游服务（需配合 `-agy`） |
| `-api-key` | 读取 `GATEWAY_API_KEY` | Bearer 鉴权密钥 |
| `-model-map` | `{}` | 模型别名映射，例如 `'{"gpt-4o":"实际模型ID"}'` |
| `-agent` | `gateway-clean` | 指定 ACP Agent；传 `default` 可恢复 `agy` 原生代理 |
| `-soft-block-residual-tools` | `true` | 软屏蔽 `agy` 无法从底层移除的残留内置工具名 |
| `-max-sessions` | `64` | 最大内存会话数 |
| `-max-concurrent-turns` | `8` | 最大并发执行回合数 |
| `-session-idle-ttl` | `20m` | 空闲会话过期时间 |
| `-tool-timeout` | `5m` | 等待客户端回传工具结果的超时时间 |
| `-turn-timeout` | `15m` | 单回合最大超时时间 |
| `-body-limit` | `32 MiB` | HTTP 请求体大小上限 |
| `-cors-origins` | 空 | 允许的 CORS Origin（逗号分隔） |

---

## 开发与离线测试

仓库测试均在完全离线环境下运行，不会向真实 Google 服务发起请求：

```bash
# 1. Go 静态检查与竞态单元测试
./scripts/verify_acp_gateway.sh

# 2. 使用本地真实 agy + 离线 Mock CCPA 集成测试（可指定 AGY_REAL_BINARY）
./scripts/verify_acp_gateway.sh --real

# 3. 本地构建 AIO / slim 镜像后执行离线容器验收
./scripts/verify_aio.sh
./scripts/verify_aio.sh --native   # Linux amd64 原生观测验收
```

---

## 许可与免责声明

本项目源码基于 [MIT License](LICENSE) 开源。

本项目仅作为本地 CLI 协议转换层；Google Antigravity CLI 及在线服务受其各自条款约束，使用前请确保你的账号及使用场景符合 [Google Antigravity 服务条款](https://www.antigravity.google/terms)。
