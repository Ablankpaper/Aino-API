# 定时能力基准测试设计

## 目标

在现有账号级“定时测试”上增加可重复的能力基准测试。管理员为账号创建一个测试计划，填写固定提示词并明确选择输出类型；系统按 Cron 定时调用该账号，保存文字、JSON、图片或真实文件产物，展示历史差异和质量结果，用于发现账号能力下降、输出不完整、格式错误、响应变慢或账号失效。

## 范围与原则

- 只允许管理员创建、修改、执行和下载基准测试结果。
- 一个计划绑定一个账号、一个模型、一个提示词和一个输出类型：text、json、image、file。
- 提示词是模型输入；输出类型是服务端执行协议，不依赖服务端猜测提示词意图。
- 定时任务只生成并保存产物，不执行模型生成的代码或脚本。
- 结果保留数量、单次超时、输出大小和并发数均有上限，防止小规格实例被任务拖垮。
- 现有“连接测试”计划继续兼容，历史计划默认视为 connectivity 类型。

## 用户流程

管理员从账号操作菜单打开“定时测试”，点击新增计划，选择“能力基准测试”，填写模型和提示词，选择输出类型，按类型填写校验规则，再设置 Cron、超时和保留数量。保存后可以立即执行一次建立基线。计划列表显示最近状态、最近耗时和最近一次质量检查；展开计划可查看历史结果、文本差异、JSON 差异、图片缩略图或文件下载。

管理员可暂停计划、编辑提示词、复制计划、立即执行或删除计划。编辑提示词或输出类型后，系统将结果序列标记为新基线，避免把不同测试混在同一条趋势中。

## 数据模型

新增迁移 backend/migrations/240_add_scheduled_capability_benchmark.sql：

### scheduled_test_plans 扩展

- test_type VARCHAR(20) NOT NULL DEFAULT 'connectivity'
- prompt TEXT NOT NULL DEFAULT ''
- output_type VARCHAR(20) NOT NULL DEFAULT 'text'
- validation_config JSONB NOT NULL DEFAULT '{}'
- timeout_seconds INT NOT NULL DEFAULT 120
- max_output_bytes INT NOT NULL DEFAULT 10485760
- baseline_version BIGINT NOT NULL DEFAULT 1

约束：连接测试的提示词为空且输出类型固定为 text；能力基准测试必须有非空提示词，输出类型只能是四种受支持值；超时范围为 10–600 秒；单次产物大小范围为 1 KiB–64 MiB；保留数量继续受现有计划上限校验。

### scheduled_test_results 扩展

- test_type VARCHAR(20) NOT NULL DEFAULT 'connectivity'
- output_type VARCHAR(20) NOT NULL DEFAULT 'text'
- response_text TEXT NOT NULL DEFAULT ''
- normalized_text TEXT NOT NULL DEFAULT ''
- validation_status VARCHAR(20) NOT NULL DEFAULT 'not_checked'
- validation_message TEXT NOT NULL DEFAULT ''
- token_usage JSONB NOT NULL DEFAULT '{}'
- content_sha256 CHAR(64) NOT NULL DEFAULT ''
- baseline_version BIGINT NOT NULL DEFAULT 1

### scheduled_test_artifacts

- id BIGSERIAL PRIMARY KEY
- result_id BIGINT NOT NULL REFERENCES scheduled_test_results(id) ON DELETE CASCADE
- relative_path TEXT NOT NULL
- file_name VARCHAR(255) NOT NULL
- media_type VARCHAR(150) NOT NULL
- byte_size BIGINT NOT NULL
- sha256 CHAR(64) NOT NULL
- created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()

文件保存到配置的数据目录下的 scheduled-tests/<plan-id>/<result-id>/，数据库只保存相对路径和校验信息。下载接口通过结果归属校验和管理员鉴权读取文件，禁止使用用户提供的路径跳出该目录。

## 执行架构

保留现有每分钟扫描的 ScheduledTestRunnerService，把 runOnePlan 委托给 CapabilityBenchmarkService：

1. 加载计划和账号，校验计划版本、启用状态与资源限制。
2. 根据输出类型选择处理器：文本处理器调用文本生成接口；JSON 处理器使用结构化响应并解析 JSON；图片处理器调用图片生成接口并验证图片格式；文件处理器要求模型返回文件名、MIME 类型和内容，解码后写入安全目录。
3. 在计划指定超时内完成调用，记录开始/结束时间、耗时、状态和 token 用量。
4. 运行类型校验和可选规则校验，计算规范化文本及 SHA-256；二进制产物只保存元数据和文件，不把大内容重复写入结果表。
5. 写入结果和产物记录，按 max_results 清理旧结果及对应文件。
6. 更新计划的 last_run_at、next_run_at；失败或校验不通过仍然推进下一次运行，避免计划卡死。

所有处理器共享一个小的接口，例如 Run(ctx, plan, account) (BenchmarkOutput, error)，但各自负责协议解析和类型校验。执行并发继续受现有全局 worker 数量限制，并新增单计划互斥，避免手动立即执行和 Cron 同时消耗一个账号。

## 校验与基线

基础校验始终执行：上游请求成功、响应未超时、产物非空、字节数不超过计划限制、输出类型符合协议。管理员可为计划配置以下可重复规则：

- 文本：最小字符数、必需关键词、正则表达式。
- JSON：必须可解析、必需字段、字段类型和可选 JSON Schema 子集。
- 图片：必须可解码、允许的 MIME、最小宽高和最大文件大小。
- 文件：文件名后缀、MIME、最小大小、文本文件编码和可选关键词。

历史比较以同一 baseline_version 为前提：文本/JSON 比较规范化文本和结构差异，图片/文件比较 SHA-256、大小、MIME、图片尺寸或文本摘要。系统报告“成功/失败/校验不通过”，不输出主观的智力分数；耗时和错误率单独形成趋势。管理员修改提示词、模型或输出类型时递增 baseline_version。

## API 与管理界面

扩展现有 /admin/scheduled-test-plans 的创建和更新请求，增加 test_type、prompt、output_type、validation_config、timeout_seconds、max_output_bytes。增加：

- POST /admin/scheduled-test-plans/:id/run：立即执行并返回结果摘要。
- GET /admin/scheduled-test-plans/:id/results/:resultId：读取完整结果和产物元数据。
- GET /admin/scheduled-test-plans/:id/results/:resultId/artifacts/:artifactId：下载产物。

保留现有列表和结果接口，响应增加类型、校验和基线字段。前端 ScheduledTestsPanel.vue 增加类型选择器、提示词编辑器、按类型显示的校验配置、立即执行按钮和结果预览。图片使用缩略图，文件显示名称、MIME、大小、哈希和下载按钮；大文本默认折叠，避免一次加载过多数据。

## 错误处理与通知

错误分为连接失败、上游协议错误、产物写入错误和质量校验不通过，均保存可读的 error_message 或 validation_message，不保存账号密钥。通知复用现有运维通知通道，只在连续失败阈值达到或从成功变为失败时发送；恢复时发送一次恢复通知。单次任务达到超时后取消上下文并释放 worker。

## 安全、容量与迁移

- 提示词、结果和文件只对管理员开放；下载响应使用 Content-Disposition: attachment 和存储的安全文件名。
- 文件路径由服务端生成，拒绝绝对路径、目录穿越和符号链接；清理时只删除计划专属目录。
- 默认最大输出 10 MiB，图片和文件可提高到 64 MiB；默认超时 120 秒；默认保留 50 条结果。
- 产物目录位于现有 data 持久化卷，备份与迁移沿用现有数据目录策略。
- 迁移脚本为幂等 SQL；旧连接测试计划和结果无需转换即可继续运行。

## 测试与验收

- 后端单测覆盖四种处理器的成功、超时、格式错误、大小限制、路径穿越和结果清理。
- 仓储测试覆盖迁移字段、结果与产物级联删除、基线版本更新。
- HTTP 测试覆盖管理员鉴权、计划创建/更新校验、立即执行和产物下载。
- 前端测试覆盖类型切换、校验规则序列化、历史结果展示和下载链接。
- 验收标准：同一账号同一计划可按 Cron 运行；四种输出类型均能保存并预览/下载；故意返回错误格式时状态为校验不通过；修改提示词后历史结果分段；超时和失败会继续计算下一次运行；旧连接测试行为不变。

## 不在本次范围

- 不自动判断提示词意图并切换输出类型。
- 不执行生成的代码、命令、宏或工作流。
- 不引入独立队列、分布式 Worker 或外部对象存储。
- 不用另一个模型给结果打主观“智力分数”。
