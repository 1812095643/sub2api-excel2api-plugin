# 更新记录

## 0.6.0

- 固定使用 Excel2API BPS 工具隧道，不拆分多种路由模式。
- 加入来自 openai/codex 固定提交的内置工具目录。
- 支持 function、custom、namespace 工具声明和回放。
- 严格处理嵌套 JSON、工具 schema、原始 BPS 调用项和工具结果续接。
- 工具结果后的空回合最多自动重试一次。
- 安装包附带 Codex 工具目录快照、来源提交号和 Apache License 2.0。
- 保留请求时区、英文 Accept-Language、并行降智检测、Cron、条件任务和 Excel2API 专用测试。
- 修复用户消息中的 data:image 被 BPS 拒绝的问题：新增同源附件上传、file_id 替换和账号级图片缓存。
- 保留工具结果中的截图格式，不把工具返回图片误当成用户附件上传。

## 0.4.0

- 增加 Excel2API BPS 请求转换和工具隧道的基础实现。
- 增加请求时区、图片 URL 校验和插件内专用测试页面。

## 0.3.0

- 增加请求时区、并行检测、Cron/条件任务和指定账号 Excel2API 开关。
