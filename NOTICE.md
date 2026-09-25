本插件为个人构建，不代表 Sub2API 或 Sub4API 官方发布。

网络传输子进程来自既有 OpenAI OAuth Transport 包，字节保持原样。插件实现请求时区、Accept-Language 统一、可启停的 ModelTrace 并行定时检测，以及按账号配置的 Excel2API BPS Responses 工具桥接；网络请求使用 Go 标准 HTTP 客户端。

Codex CLI 工具目录取自 https://github.com/openai/codex 的提交 5c8fc15cc99241d3a1a415a629841540cdff4b28，依据 Apache License 2.0 复用工具名称、类型、描述和 JSON Schema。插件不包含 Codex CLI 的工具执行器；执行仍由客户端完成。

降智检测算法与挑战模板依据 Sub4API v1.1.3 中的 ModelTrace 实现，相关许可见 internal/modeltrace/modeltrace_LICENSE。Sub2API v0.2.7 插件协议及其依赖按各自上游许可使用。
