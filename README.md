# Sub2API Excel2API 插件

这是一个面向 Sub2API 0.2.7 的 OpenAI OAuth Transport 插件。它把指定的 OpenAI OAuth 账号接入 ChatGPT Excel 后端 `https://bps.openai.com/basispoints/api/responses`，并在 BPS 不接受客户端原生 `tools` 的情况下，通过固定的 `run_officejs` 隧道保持 Codex CLI 工具调用协议。

当前版本：`0.7.0`

最新版安装包：`dist/openai-account-health-0.7.0-linux-amd64.s2plugin`

SHA-256：见 `dist/checksums-openai-account-health-0.7.0.txt`

## 能做什么

- **Excel2API**：按账号 ID 选择需要走 BPS 的 OpenAI OAuth 账号，自动从 Sub2API HostService 读取 access token、ChatGPT account ID 和账号代理。
- **Codex 工具目录**：内置来自 `openai/codex` 固定提交的 35 个可固化工具/命名空间声明，包含 `exec_command`、`apply_patch`、`view_image`、`update_plan`、用户询问、MCP 资源、协作工具、代码模式和图片工具等。
- **BPS 工具隧道**：客户端工具声明写入 developer 目录，BPS 只接收原生 `run_officejs`；插件解析 `code` 中的嵌套 JSON，再还原为客户端可执行的 `function_call` 或 `custom_tool_call`。
- **稳定续接**：保存原始 BPS 调用项，保留 `id`、`call_id`、`summary`、`references` 等字段；固定 `turn_id`，工具结果只递增 `agent_iteration`。
- **错误防护**：按调用 ID 维护工具状态，完整读取并校验工具参数；半截 JSON、未知工具、schema 不匹配和连续空回合会明确报错，工具结果后的空回合最多自动重试一次。
- **请求时区**：默认 `Asia/Singapore`，支持按账号覆盖；已有 `Accept-Language` 会统一为英文。
- **降智检测**：多个模型并行测试、总开关、指定账号、只测试启用调度账号、Cron 和条件任务。
- **图片上传**：用户消息中的 `data:image` 会上传到 BPS 同源附件接口并替换为 `file_id`；工具结果里的截图保持原格式回放。上传失败会返回明确错误。

工具执行器仍由 Codex CLI/Sub2API 客户端执行。插件不会执行 shell、`apply_patch`、MCP 或图片工具，也不会把 OAuth Token 写入配置、UI 或日志。

## 兼容范围

| 项目 | 要求 |
| --- | --- |
| Sub2API | `>=0.2.7 <0.3.0` |
| 推荐版本 | `0.2.7` |
| 平台 | Linux amd64 |
| 插件能力 | `openai.oauth.outbound_transport.v1` |
| 账号类型 | OpenAI OAuth |
| 发布签名 | Ed25519，key id `personal-astra-transport-20260921` |

当前包只提供 Linux amd64 运行时。API Key 账号、其他 Provider 和 OAuth 刷新流程继续由 Sub2API 原有路径处理。

## 安装

1. 打开 Sub2API 管理后台的插件管理页。
2. 上传 `dist/openai-account-health-0.7.0-linux-amd64.s2plugin`。
3. 确认签名状态为可信，然后启用插件。
4. 打开插件配置页，开启 **Excel2API**，逐行填写账号 ID。
5. 开启 **启用测试**，填写至少一个检测模型并保存。
6. 点击 **保存并测试 Excel2API**，确认 BPS、账号授权和代理链路可用。
7. 在 Codex CLI 或经过 Sub2API 的 Responses 客户端中使用指定账号。

账号管理页的“测试连接”不属于本插件配置流程；Excel2API 使用插件页面内的专用测试按钮。

## 配置项

插件 UI 保存的是以下 JSON 字段，字段名保持 `snake_case`：

| 字段 | 类型 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `default_timezone` | string | `Asia/Singapore` | 默认 IANA 时区 |
| `account_timezones` | object | `{}` | `账号 ID -> IANA 时区` |
| `direct_enabled` | boolean | `false` | Excel2API 总开关 |
| `direct_account_ids` | number[] | `[]` | 走 BPS 的 Sub2API OpenAI OAuth 账号 ID |
| `diagnostic_enabled` | boolean | `true` | 降智检测总开关 |
| `diagnostic_models` | string[] | `[]` | 每个模型单独发起正式 Responses 请求 |
| `diagnostic_account_ids` | number[] | `[]` | 留空表示全部 OpenAI OAuth 账号 |
| `diagnostic_schedulable_only` | boolean | `false` | 只检测宿主判定为启用调度的账号 |
| `diagnostic_concurrency` | integer | `4` | 并行度，范围 1 到 16 |
| `schedule_mode` | string | `disabled` | `disabled`、`cron` 或 `condition` |
| `schedule_cron` | string | `""` | 五段 Cron，例如 `*/30 * * * *` |
| `schedule_condition` | string | `has_schedulable_accounts` | 条件任务：可调度账号出现或启动时执行 |

示例 JSON：

```json
{
  "default_timezone": "Asia/Singapore",
  "account_timezones": {
    "123": "America/Los_Angeles",
    "456": "Asia/Tokyo"
  },
  "direct_enabled": true,
  "direct_account_ids": [123, 456],
  "diagnostic_enabled": true,
  "diagnostic_models": ["gpt-5.6-sol", "gpt-6-astra"],
  "diagnostic_account_ids": [],
  "diagnostic_schedulable_only": true,
  "diagnostic_concurrency": 4,
  "schedule_mode": "cron",
  "schedule_cron": "*/30 * * * *",
  "schedule_condition": "has_schedulable_accounts"
}
```

## YAML 配置方式

Sub2API 0.2.7 的插件自定义配置不是从主 YAML 文件直接读取，而是通过插件 UI 或管理 API 保存后加密写入数据库。YAML 需要分成两部分理解：宿主信任配置可以直接写入 Sub2API 主配置，插件业务字段需要转换成 JSON 后提交。

### 1. Sub2API 主配置 YAML

把下面内容合并到服务器的主 `config.yaml`。不要把私钥写入 YAML；这里只放发布者公钥：

```yaml
plugins:
  allow_unsigned: false
  trusted_publishers:
    personal-astra-transport-20260921: "LPYLHgoVkZ4sCI1jDDlYXPkv8bErFM9mKFpN5/ngWjY="
```

`trusted_publishers` 的位置是宿主配置里的 `plugins` 节点。修改后按你的部署方式重启或重新加载 Sub2API，使新发布者公钥生效。

### 2. 插件业务配置 YAML

仓库提供 `docs/excel2api-config.example.yaml`。它用于部署记录、审计和转换，不会被 Sub2API 0.2.7 自动读取：

```yaml
plugin_id: local.personal.openai-account-health
config:
  default_timezone: Asia/Singapore
  account_timezones:
    "123": America/Los_Angeles
    "456": Asia/Tokyo
  direct_enabled: true
  direct_account_ids:
    - 123
    - 456
  diagnostic_enabled: true
  diagnostic_models:
    - gpt-5.6-sol
    - gpt-6-astra
  diagnostic_account_ids: []
  diagnostic_schedulable_only: true
  diagnostic_concurrency: 4
  schedule_mode: cron
  schedule_cron: "*/30 * * * *"
  schedule_condition: has_schedulable_accounts
```

转换后，通过管理 API 保存 `config` 对象：

```powershell
$pluginId = 1
$config = Get-Content .\docs\excel2api-config.json -Raw
Invoke-RestMethod `
  -Method Put `
  -Uri "http://127.0.0.1:8080/admin/plugins/$pluginId/config" `
  -ContentType "application/json" `
  -Body $config
```

实际部署时需要使用你的 Sub2API 管理认证和二次验证流程。`pluginId` 是后台安装后返回的数据库 ID，不是插件字符串 ID。

如果你没有 YAML 转换工具，最稳妥的方式是直接在插件 UI 填写并保存；UI 使用的字段与上面 YAML 的 `config` 节点一一对应。

### 3. 用 yq 转 JSON

安装 `yq` 后可以把示例 YAML 的 `config` 节点导出为 JSON：

```bash
yq -o=json '.config' docs/excel2api-config.example.yaml > docs/excel2api-config.json
```

导出的 JSON 可以提交到 `/admin/plugins/:id/config`。不要把 access token、refresh token、代理密码或 Cookie 写入这个 YAML/JSON。

## 请求链路

```text
Codex CLI / Responses 客户端
        |
        | 原生 tools + input
        v
Sub2API OpenAI OAuth Transport
        |
        | Excel2API 插件移除 tools，注入 Codex 工具目录
        | BPS 只收到 run_officejs
        v
bps.openai.com/basispoints/api/responses
        |
        | run_officejs.code -> {tool, namespace, args}
        v
插件还原 function_call/custom_tool_call
        |
        v
Codex CLI/Sub2API 客户端执行工具并回传 output
```

插件只处理协议，不执行工具。工具结果回传时会带回 BPS 原始调用项，避免上游把回合识别成新的计划。

## 常见问题

**为什么 BPS 请求里看不到 `tools`？**

这是设计结果。BPS 会因为客户端声明 `tools` 返回 422，所以插件把 Codex 工具目录写进 developer 消息，并使用 BPS 原生 `run_officejs` 作为固定外壳。

**为什么 `apply_patch` 是 custom？**

Codex CLI 的自由文本工具必须返回 `custom_tool_call`，JSON function 工具返回 `function_call`。插件按声明类型恢复，避免 `incompatible payload`。

**为什么工具名里有 namespace？**

Codex 的协作、代码模式和插件工具可能放在 namespace 下。插件通过 namespace 展平名定位工具，再恢复 `name + namespace`。

**图片现在怎么处理？**

BPS 拒绝标准 Responses 的 `data:image/...;base64,...` 用户图片。插件会先按 Excel 加载项的附件接口上传用户图片，再把正文中的图片替换为 `file_id`；已经是 HTTPS URL 的图片直接保留，工具返回的截图不上传并继续回放。

**为什么没有直接把 Codex 执行器复制进来？**

执行器必须运行在客户端自己的工作目录、权限和审批上下文里。插件复制执行器会破坏安全边界，也不能替代客户端的实际工具环境。

## 目录

- `internal/adapter/codex_tool_catalog.json`：Codex CLI 工具声明快照。
- `internal/adapter/basispoints.go`：BPS 工具隧道、工具类型和调用回放。
- `internal/adapter/direct.go`：Excel2API HTTP/TLS/代理链路。
- `ui/`：Sub2API 插件配置页面。
- `docs/sub2api-host-config.example.yaml`：宿主信任发布者 YAML。
- `docs/excel2api-config.example.yaml`：插件业务配置 YAML。
- `dist/`：最新版可安装插件和 SHA-256 文件。

## 开发与打包

```powershell
$env:GOPATH = (Resolve-Path ..\gopath).Path
$env:GOCACHE = (Resolve-Path ..\gocache).Path
$env:GOOS = "linux"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"
..\toolchain\go\bin\go.exe test -c ./internal/adapter -o ..\linux-test\adapter.test
..\toolchain\go\bin\go.exe build -trimpath -o runtimes\linux-amd64\openai-account-health ./cmd/astra-transport
python tools/package.py `
  --binary runtimes/linux-amd64/openai-account-health `
  --child runtimes/linux-amd64/openai-transport-original `
  --key <existing-private-key> `
  --output dist/openai-account-health-<version>-linux-amd64.s2plugin
```

私钥只用于本地或 CI 签名，不提交到仓库。生产包使用既有发布者公钥信任链。
