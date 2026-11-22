# AGY ACP Gateway

网关把 Chat Completions 和 Responses 请求转成 `agy acp` 的 stdio JSON-RPC 消息。工具在客户端执行。客户端可以用响应 ID 把结果送回原 ACP 回合，也可以在 Responses 请求中带上完整历史，由新 ACP 会话继续。实现使用 Go 标准库，目标平台为 Linux，Go 版本要求 1.23 或以上。

## 下载与许可

[GitHub Releases](https://github.com/RC-CHN/agy-acp-gateway/releases) 提供 Linux amd64 和 arm64 网关压缩包、SHA-256 校验文件，以及可以用 `docker load -i` 导入的 amd64 slim 镜像。下载对应架构的压缩包与 `SHA256SUMS`，在下载目录执行 `sha256sum -c --ignore-missing SHA256SUMS`，解压后运行其中的 `./agy-acp-gateway` 即可启动模拟服务。

```bash
docker run --rm -p 127.0.0.1:8080:8080 -e GATEWAY_API_KEY \
  ghcr.io/rc-chn/agy-acp-gateway:v0.1.0 -listen 0.0.0.0:8080
```

上面的镜像命令需要先在 shell 中导出自己的 `GATEWAY_API_KEY`。AIO 的本地构建和账号管理见下文。网关源码采用 MIT 许可证。Google Antigravity CLI 和在线服务有各自的条款，个人版条款对使用第三方软件访问服务有明确限制。接入在线服务前，请确认你的账号和场景具有所需授权。[MIT 许可证](LICENSE) · [Google Antigravity 服务条款](https://www.antigravity.google/terms)

## 本地启动

在仓库根目录运行以下命令即可使用内置 mock。默认监听 `127.0.0.1:8080`。这个模式使用随网关一起构建的模拟 ACP 子进程。

```bash
go run -buildvcs=false ./agy_acp_gateway
```

```bash
curl -s http://127.0.0.1:8080/ready
curl -s http://127.0.0.1:8080/v1/models
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"mock","messages":[{"role":"user","content":"hello"}],"stream":true}'
```

下面是可直接执行的工具回传示例，使用 `jq` 从第一轮提取响应 ID 和工具调用 ID。mock 收到以 `tools:` 开头的文本时会调用请求里注册的所有工具。

```bash
api=http://127.0.0.1:8080/v1/responses
first=$(curl -fsS "$api" -H 'Content-Type: application/json' -d '{
  "model":"mock","input":"tools: weather",
  "tools":[{"type":"function","name":"get_weather","parameters":{
    "type":"object","properties":{"city":{"type":"string"}}}}]
}')
jq -c '{previous_response_id:.id,input:[.output[]|select(.type=="function_call")|
  {type:"function_call_output",call_id:.call_id,output:"Shanghai, 18 C"}]}' <<<"$first" |
  curl -fsS "$api" -H 'Content-Type: application/json' --data-binary @-
```

## Responses 无状态续聊

每轮传 `store: false`，省略 `previous_response_id`。客户端保留上次的 `input`，把返回的整个 `output` 按顺序追加进去，再追加下一条用户消息或工具结果。JSON 响应取 `output`，SSE 取终态事件里的 `response.output`。这份历史可以用于重试、并发分支，也可以交给刚重启的网关。[OpenAI 会话状态](https://developers.openai.com/api/docs/guides/conversation-state) · [OpenAI 函数调用](https://developers.openai.com/api/docs/guides/function-calling)

下面的 Python 3 示例只需标准库和默认启动的本地 mock。它从第一轮返回的调用中取出 `call_id`，交回结果后会打印包含 `Shanghai, 18 C` 的回答。

```python
import json
from urllib.request import Request, urlopen

def respond(history, tools):
    body = {"model": "mock", "store": False, "input": history, "tools": tools}
    request = Request("http://127.0.0.1:8080/v1/responses",
                      data=json.dumps(body).encode(),
                      headers={"Content-Type": "application/json"})
    with urlopen(request) as response:
        return json.load(response)

history = [{"role": "user", "content": "tools: weather"}]
tools = [{"type": "function", "name": "get_weather",
          "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}]
while True:
    response = respond(history, tools)
    history.extend(response["output"])
    calls = [item for item in response["output"] if item["type"] == "function_call"]
    if not calls:
        print([p["text"] for item in response["output"] if item["type"] == "message"
               for p in item["content"] if p["type"] == "output_text"])
        break
    for call in calls:
        history.append({"type": "function_call_output", "call_id": call["call_id"],
                        "output": "Shanghai, 18 C"})

# 下一轮文本续聊可继续使用同一份 history
history.append({"role": "user", "content": "那明天呢？"})
next_response = respond(history, tools)
```

| 本次请求 | 网关行为 |
| --- | --- |
| 完整 `input`，`store: false` | 每次新建 ACP 会话，返回后释放网关内存中的会话、待处理调用和 ID 映射。返回的响应 ID 再用于续接会得到 400 |
| 完整 `input`，`store: true` 或省略 | 按该历史创建独立会话，生成的响应 ID 可用于下一次续接 |
| `previous_response_id`，`store: true` 或省略 | 按当前网关进程中最新且有效的 ID 续接。只交回原批次工具结果时通过原生 MCP 回调继续。成功后新 ID 取代旧 ID |
| `previous_response_id`，`store: false` | 从有效父响应的历史另开一次性分支。父 ID 仍按原来的有效期和容量策略管理，分支 ID 用后即失效。没有可供分支使用的槽位时返回 429 |

同一批次的工具结果可乱序回传，使用 `call_id` 配对，每个调用交回一次。无状态请求只带孤立的结果、复用调用 ID、漏交结果或提前插入下一条用户消息都会返回 400。网关重放已有记录时不会执行客户端工具。客户端需要自行保存已执行的结果，有副作用的工具尤其需要避免因 HTTP 重试而重复执行。无状态请求也应带上本轮需要的工具、模型和指令。整份历史受请求体上限约束。

新建的 ACP 会话通过提示内容接收可见的角色、文本、推理摘要、函数参数和结果，图片作为原生图片块送入。原会话的隐藏推理签名、缓存和运行中的工具回调无法从客户端历史恢复。仅含密文的推理项、`item_reference` 和服务端 `conversation` 引用会返回 400。无状态工具轮只保留已交付到 HTTP 响应的调用。需要恢复原生回调或继续等待后续调用时，可使用已保存的响应 ID。`store: false` 的释放范围是本 Go 网关的会话内存，`agy` 自身的文件和上游留存策略另行管理。[OpenAI 推理与无状态模式](https://developers.openai.com/api/docs/guides/reasoning)

## 运行模式

| 模式 | 如何选择 | 行为 |
| --- | --- | --- |
| 模拟 ACP | 默认 | 本地 echo、SSE 和工具回传，可以联调客户端 |
| 真实 ACP 和本地 mock 上游 | `-agy <可执行文件> -mock-ccpa http://127.0.0.1:<端口>` | 使用临时工作区、临时 HOME 和合成凭据。验收脚本会自行启动 mock CCPA |
| 原生上游 | 显式同时指定 `-agy` 和 `-live` | 由官方 CLI 使用其配置的身份和服务。v0.1.0 已使用 agy 1.5.14 验证 Hydrogen High 的 Chat、Responses 和流式生成 |

真实进程使用 `--process-per-session`。未指定 `-agent` 时，网关会在本次临时工作区内自动生成极简自定义代理 `.agents/agents/gateway-clean.md`（`excludeDefaultComponents: true`、空正文、占位 `vbridge` MCP 声明）并以 `--agent gateway-clean` 启动 `agy acp`：
- **默认系统提示词**：彻底移除 Antigravity 默认 12KB 编程系统提示词（不套 `<System Prompt>` 包装）。
- **内置工具**：移除了全部 9 个内置编码/系统工具（`read_file`, `run_command`, `write_file`, `edit_file`, `search_web`, `generate_image`, `schedule`, `send_message`, `ask_question`）。受 `agy` v1.5.14 二进制限制，纯聊天仍保留精确名为 `manage_task` 的底层基础工具，带 MCP 工具时会附带精确名为 `list_resources`、`read_resource` 的 MCP 资源工具（客户端工具本身统一以 `mcp_vbridge_` 前缀暴露）。
- **残留工具软屏蔽（默认开启）**：网关默认在 ACP `session/new` 的 `systemPrompt.append` 末尾按需追加一句极简提示，严格指明上述精确工具名不可用（纯聊天仅屏蔽 `manage_task`，带工具时屏蔽 `manage_task`、`list_resources`、`read_resource`）。如需恢复严格 0 字节系统提示词透传，可传 `-soft-block-residual-tools=false`。
- **权限回调兜底**：ACP 请求权限时，只对当前会话已注册的、由 worker 解析出的完整 MCP 工具名（`mcp_vbridge_*`）批准一次；`-agent default`（或账号未开启 `enable-customization-kind-agent` 回退到默认代理）时的内置工具调用仍全部拒绝。显示标题和工具入参不参与授权。如需恢复 `agy` 原生默认代理，可显式传 `-agent default`。

## 兼容范围

| 接口 | 行为 |
| --- | --- |
| `POST /v1/chat/completions` | JSON 或 SSE，文本、思考增量、函数调用、用量统计，支持 `stream_options.include_usage` |
| `POST /v1/responses` | JSON 或语义 SSE，支持完整历史重放、`store: false`、`previous_response_id`、函数结果、推理摘要、终态和递增事件序号 |
| `GET /v1/models` | 返回实际 ACP 模型目录和可用别名 |
| `GET /health`、`GET /ready` | 返回进程状态和会话计数。worker 初始化中或容量已满时，ready 返回 503 |

- Chat 按显式会话键或首条用户消息等字段定位候选会话，再检查完整历史前缀。修改历史、系统指令或工具定义会重建上下文；工具结果回传优先命中原生挂起 MCP 回调，若原会话已过期、被回收或跨网关重启，则在校验完整 `tool_calls`/`tool` 配对后自动折叠历史重建会话继续。
- Responses 支持从会话最新响应继续。旧分支、过期 ID 和未知 ID 返回 400。普通新回合省略 `instructions` 时会去掉上一轮指令。等待工具结果期间，原 ACP 回合保持原有设置。
- 同一会话同时处理一个客户端请求，竞争请求返回 429。有状态模式下，等待工具的会话释放 HTTP 请求占用，同时保留 ACP 回合和已公布的调用 ID。工具结果可乱序提交。缺失、重复或未知 ID 返回 400，客户端可以更正后重试。
- 在 40 ms 窗口内到达的并行调用合并返回。有状态回合中较晚到达的调用可在下一次结果回传后继续交付。工具等待超时、客户端中途断开、子进程退出时会清理关联状态。工具挂起这一段还没有最终 ACP usage。
- 支持 PNG、JPEG、WebP、GIF 的 base64 data URL。每请求图片解码后的总量上限为 20 MiB，远程图片 URL 返回 400。Chat 的 `reasoning_content` 是扩展字段。Responses 通过 `reasoning_summary_text` 事件输出摘要。
- system、developer 和 instructions 使用 ACP 的系统指令追加功能。思考等级按会话返回的选项选择。ACP 未提供的采样参数（例如 `temperature`、`top_p`、`max_output_tokens` 和 `stop`）不会约束生成。这一版工具接口支持 `type: function`。

## 配置与运行

可用 `go run -buildvcs=false ./agy_acp_gateway -h` 查看参数。常用配置包括 `GATEWAY_API_KEY`、`-model-map '{"alias":"实际模型 ID"}'`、`-max-concurrent-turns`（8）、`-max-sessions`（64）、`-session-idle-ttl`（20m）、`-tool-timeout`（5m）、`-turn-timeout`（15m）、`-body-limit`（32 MiB）和 `-cors-origins`。网关的结构化请求日志记录请求标识、端点、状态与耗时，省略正文和图片。

### AIO 镜像、Dashboard 与环境变量

Dockerfile 和 Compose 默认构建 AIO。构建阶段从官方安装器下载当前平台的 agy，由安装器校验发布载荷，再检查最终 ELF 和版本。可以用 `AGY_EXPECTED_VERSION` 和 `AGY_EXPECTED_SHA256` 锁定验收值。需要查询最新清单时可用 `docker buildx build --no-cache-filter agy-fetch --load -t agy-acp-gateway:aio .`。自建安装源可显式配置 `AGY_INSTALL_URL`。[官方安装器](https://antigravity.google/cli/install.sh) · [Dockerfile](Dockerfile)

Compose 读取宿主机已导出的环境变量和项目根目录的 `.env`，同名项以宿主机环境为准。复制示例后填入自己的网关密钥，再按需要启用代理。[环境变量示例](.env.example) · [Compose 配置](docker-compose.yml)

```bash
cp .env.example .env
# 编辑 .env，设置 GATEWAY_API_KEY，再构建
docker compose build
```

| 变量 | 用途 |
| --- | --- |
| `GATEWAY_API_KEY` | 网关 Bearer 密钥，Compose 必填。用于 Dashboard 和两个模型 API 的本地鉴权。仅传入运行容器 |
| `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`、`NO_PROXY` | Compose 同时传给构建阶段和运行容器。支持对应小写形式。原生 worker 和登录子进程继承代理，自动将两份 NO_PROXY 规则合并并补齐回环地址 |
| `GATEWAY_CREDENTIAL_MODE` | 默认 `managed`，允许网页登录、切号和登出。`external` 适用于由宿主机维护的只读凭据，四个认证写接口返回 403 |
| `GATEWAY_AUTH_KEEPALIVE_INTERVAL` | 已登录的 managed 实例默认每空闲 `30m` 检查一次。可填 `15m` 等 Go 时长，`0` 关闭后台检查。命令行参数为 `-auth-keepalive-interval` |
| `AGY_DATA_VOLUME` | Compose 默认使用当前项目的 `<项目名>_agy-data` 命名卷。更改项目名或部署目录时，填原卷的实际名称即可继续使用其中的登录态 |
| `GATEWAY_DASHBOARD` | 默认 `true`。`false` 关闭首页和管理路由。直接运行二进制可用 `-credential-mode`、`-dashboard` 覆盖相应环境变量 |
| `AGY_EXPECTED_VERSION`、`AGY_EXPECTED_SHA256` | 可选构建参数。摘要为目标架构最终 ELF 的 SHA-256，版本或摘要不一致会令构建失败 |

代理 URL 应从构建容器及运行容器可达。例如可以设置 `HTTPS_PROXY=http://proxy.internal.example:3128`。容器内的 `127.0.0.1` 指向容器自身。原生 CLI 登录后负责上游 Google 凭据，`GATEWAY_API_KEY` 只用于访问本地网关。

**先离线体验。** 以下命令用内置 mock 启动同一份 Dashboard 和模型接口。打开 `http://127.0.0.1:8080/`，输入网关密钥即可查看状态、计数和接入示例。

```bash
docker compose run --rm --service-ports acp-gateway -listen 0.0.0.0:8080
```

**确认启用真实上游后**，执行 `docker compose up -d`。容器默认以 UID 65532 运行，仅在宿主机回环发布 8080，`agy-data` 命名卷持久化 `/data`。网页登录时点“开始 Google 登录”，在官方页面授权后把授权码或回调 URL 粘回 Dashboard，再等待 ACP 就绪。已有账号时，取消切号会保留原会话。提交新授权或登出会终止旧会话，旧的 Responses ID 和工具回调随即失效。单文件凭据挂载应设置 `GATEWAY_CREDENTIAL_MODE=external`，挂载位置为 `/data/.gemini/antigravity-cli/antigravity-oauth-token`。该文件须对运行 UID 可读，父目录须可写，以便 CLI 创建缓存。

**空闲时也会续期。** 已保存登录的 managed 网关就绪后，即使从启动起一直没有客户端请求，也会定时启动一个短生命周期的原生 agy 会话。agy 按需刷新 OAuth 并将轮换后的凭据写回自己的存储。检查可能经配置的代理访问认证和模型目录服务，模型生成次数与用量均为零。在途请求和待回传的工具调用会推迟检查。检查失败后自动退避重试，默认从 1 分钟开始，最长 30 分钟。正常请求仍能触发 agy 自身的续期。上游撤销授权后需要重新授权。

重建或替换 AIO 容器时保留同一个可写的 `/data` 卷。直接用 `docker run` 启动时，在每次启动命令中使用相同的 `-v agy-data:/data`。`docker compose down -v` 会删除 Compose 管理的卷及其中的登录态。使用 external 单文件只读挂载时，刷新凭据的持久化由宿主机负责。Responses ID 的空闲寿命由独立的 `-session-idle-ttl` 控制，默认 20 分钟。

管理状态入口为 `GET /admin/api/status`。其中 `auth.keepalive_enabled` 表示是否启用空闲检查，`last_silent_check_at` 和 `last_silent_check_error` 分别记录最近一次完成检查的时间和诊断码。Dashboard 仅将密钥保存在当前标签页的 sessionStorage。等待账号认证时 `/health` 返回 200，`/ready` 和模型接口返回 503。统计覆盖 JSON、SSE、输入错误和客户端断连。工具回合只按 ACP 最终报告的 usage 累计一次，进程重启会清空计数。

Compose 使用本机镜像标签 `agy-acp-gateway:aio`。编译阶段默认镜像是 `golang:1.26-bookworm`，本地可通过 `GO_BUILD_IMAGE` 选择缓存的 Go 1.26 构建镜像。

轻量镜像可用 `docker build --target slim -t agy-acp-gateway:slim .` 构建，默认运行内置 mock。直接使用 `docker run` 时可以用 `--env-file .env` 或 `-e GATEWAY_API_KEY -e HTTPS_PROXY -e NO_PROXY` 传入配置。

## 离线验收

```bash
./scripts/verify_acp_gateway.sh
./scripts/verify_acp_gateway.sh --real
```

第一条命令执行 vet 和带竞态检测的测试，覆盖协议、错误输入、并发、分支、超时、断开、TTL/LRU、worker 被强杀后的 502 和恢复。测试使用 Go 管道和内存 HTTP 传输。第二条另行编译 CLI 和测试程序，在只有回环接口的 Linux 环境中使用本地 `agy` 二进制（可通过 `AGY_REAL_BINARY` 指定，未指定时使用 `PATH` 中的 `agy`）。在普通 Linux 主机上脚本通过 `unshare` 建立临时网络命名空间，需要系统支持非特权用户命名空间和 `ip`。也可以在已有的离线测试环境中直接运行。

真实二进制用例核对发送给 mock CCPA 的模型请求与工具结果，包括多轮 sessionId、系统指令、图片字节、缓存 token、两个 API 的工具回传和 SSE。验收还会启动完整网关 CLI，分别发送 SIGINT 和 SIGTERM，检查子进程已被回收。无状态重启用例在第一个网关进程取得工具调用，关闭进程后，用新的网关、真实 `agy` 和独立 HOME 重放。mock CCPA 检查收到的实际会话 ID、函数参数、结果和图片，并按客户端提交的值作答。全部验收使用合成身份和本地结果。

v1.5.14 联调中的三个细节直接影响工具能否工作。mock 的 `listExperiments` 返回启用 `enable-customization-kind-mcp` 和 `enable-customization-kind-agent` 的配置。桥在工具定义的 `_meta` 中声明 `antigravity/alwaysLoad: true`。模拟模型也要遵循实际声明的参数，包含 `agy` 添加的 `toolSummary`。最终验证既检查客户端收到的原始函数名与参数，也检查客户端结果进入下一次模型请求的 `functionResponse`。

### 两个生成接口的错误响应

Chat Completions 和 Responses 会在鉴权及解析之前生成 `X-Request-Id`。非流式失败，以及开流前的失败，使用对应的 HTTP 状态，JSON 的 `error` 对象包含 `code`、`type`、`message`、`param`。`code` 是稳定的机器码，旧客户端若比较 `Bad Request` 等 HTTP 短语，需要改用机器码或整数 HTTP 状态。工具历史错误的 `param` 指向 `messages` 或 `input`，失效的续聊 ID 指向 `previous_response_id`。

已提交 SSE 响应头时，HTTP 状态保持 200。Chat 在错误帧后发送一次 `[DONE]`。Responses 以 `response.failed` 结束，保留响应 ID、序号和已有输出，SDK 可以读取 `response.error`，扩展字段 `gateway_error.status` 与 `gateway_error.error` 给出逻辑 HTTP 状态和完整错误。原生 ACP 错误使用 `error.upstream.acp_code` 保留有符号数值，认证失败 `-32000` 对外为 503 / `acp_auth_required`。客户端仍连接时，agy 主动取消会产生 502 / `acp_request_cancelled`；网关自己超时为 504 / `upstream_timeout`。认证等待期间 `/health` 保持 200，`/ready` 返回 503，两者都有 `auth_required`。[分类实现](agy_acp_gateway/errors.go) · [接口验收](agy_acp_gateway/error_contract_test.go)

### 可选的模型服务错误映射

对 Linux amd64 的 `agy 1.5.14` 构建 `37873b80…d8c94`，启动参数 `-agy-status-python /absolute/path/to/python3` 可启用只读会话库观测。需要 Python 3.11+ 标准库的 sqlite3。每个回合在原生 ACP 结束时核对会话、错误步骤及模型生成记录，由 Go 网关生成公共错误。举例来说，模型权限拒绝返回 HTTP 403 和 `upstream_permission_denied`；上游 401 返回网关 503 和 `upstream_auth_failed`，两者都保留 `error.upstream.provider_http_status`。已校验的提供方枚举在 `error.upstream.provider_status`。终态 429、503 若携带可信的 `Retry-After` 或结构化 `google.rpc.RetryInfo`，错误体带整数 `error.retry_after_s`，还能写 HTTP 头时同步设置 `Retry-After`。已开流的 Responses 在 `gateway_error.error` 下读取该字段。重试成功仍返回正常输出。默认模式直接使用 ACP，完整镜像无需安装 Python。

另一项 `-agy-frida-python /absolute/path/to/python` 只供本地 mock CCPA 做内存对照。压测中出现过 worker 异常退出，常规错误映射使用上面的会话库读取器。两个选项都会核对完整二进制哈希。[Go 分类](agy_acp_gateway/observed_error.go) · [只读适配器](agy_acp_gateway/observer/bridge.py)

在仓库根目录执行下面的命令，会在回环网络中运行带错误映射的完整 CLI 验收。设置 `AGY_FRIDA_PYTHON` 后还会单独运行内存探针对照组。

```bash
AGY_STATUS_PYTHON="$(command -v python3)" ./scripts/verify_acp_gateway.sh --real
```

### 本次验收结果（2026-11-22）

| 检查 | 结果 |
| --- | --- |
| Go 静态检查和竞态测试 | `go vet` 通过，`go test -race -shuffle=on -count=10 -cover ./...` 通过。该次 mock 测试的语句覆盖率为 80.5% |
| 真实 `agy` v1.5.14 + mock CCPA | 带竞态检测的离线集成测试连续通过 3 次，覆盖两套 API 的工具回传和 SSE、无状态跨进程重放、同一调用的独立分支和连续两轮工具调用 |
| 权限回调 | 实际发起的内置 `write_file` 和 `run_command` 均被拒绝，临时目录中的检测文件未生成 |
| 完整 CLI | 默认 mock 模式可启动、处理 HTTP 请求，并在 SIGINT 和 SIGTERM 后回收子进程 |
| Dashboard | Chromium 桌面与 375px 移动宽度验证了解锁、刷新、示例复制和账号操作。登录表单使用合成事务，真实 CLI 在隔离环境验证了生成授权页和取消 |
| Docker | 已从官方安装器构建 Linux amd64 AIO，并构建 slim。`./scripts/verify_aio.sh` 对本地 `agy-acp-gateway:aio`、`agy-acp-gateway:slim` 两个镜像执行无外网验收，覆盖 UID、数据卷、外部只读挂载、健康检查和网关鉴权配置 |


## 持续集成与发布

推送到 main 和提交 PR 会运行最低支持的 Go 1.23 及 Go 1.26 的静态检查、竞态测试、Compose 配置校验和独立二进制 HTTP 冒烟测试。在 Actions 的 CI 页面勾选 `native` 可另外构建 AIO 与 slim 并用临时身份执行原生 CLI 离线验收。推送形如 `v0.1.0` 的版本标签会先运行 CI，再发布 Linux 二进制、校验和和 slim 容器。[CI 工作流](.github/workflows/ci.yml) · [发布工作流](.github/workflows/release.yml)
