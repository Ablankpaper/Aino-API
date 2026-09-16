# Aino 平台接入：部署准备与回退操作单

状态：**准备文档，不是已部署回执**。2026-09-16 已核对 API 代码至 `19c43905f`；桌面配对版本仍在开发，最终 SHA/制品哈希必须从 Aino 的交付报告填写后才能发布。本文没有执行生产变更、短信、模型消费或付款。

## 1. 发布前硬性检查

先在隔离预发布环境完成手机号身份、旧账户绑定、原生登录、无 BYOK 模型工具往返、账本扣费、签名支付回调一次入账和旧数据升级验证。生产数据库、Redis、商户和短信凭据不能拿来替代测试夹具。

必须由运营/部署负责人补齐：

- 准确服务器/容器、部署方式、预发布地址、旧服务 SHA、备份及恢复演练回执。
- 阿里云审核模板全文及变量名、有效期措辞；`SignName` 按控制台原文录入，不能用资质名/模板名代替。已知资源是签名“郑州雾棠”、模板 `SMS_512380723`，仍需逐字核对；电信待验证项不能记三网通过。
- 专用服务端短信身份及随机 HMAC 密钥、有效协议/隐私 URL 和版本、支持联系渠道。
- 官方分组/模型 allowlist/默认模型/价格/赠送政策及逐模型工具和流式验收。
- 支付渠道商户/密钥/回调地址、支付币种/限额/费率和授权实测额度。
- 短信接收人/次数、模型预算、支付账户/金额以及明确操作授权。

公开 `/health` 成功只说明进程存活，不代表认证、模型或账本可用。版本号也不能替代 git SHA 与安装包/镜像哈希。

## 2. 构建与核对

API 使用 `backend/go.mod` 指定的 Go 版本，不降低版本绕过依赖；前端固定使用已验证的 pnpm 9.15.9，避免全局新版自动改写 lockfile：

```sh
git rev-parse HEAD
git status --short
make -C backend generate
git diff --exit-code -- backend/ent backend/cmd/server
corepack pnpm@9.15.9 --dir frontend install --frozen-lockfile
corepack pnpm@9.15.9 --dir frontend run typecheck
corepack pnpm@9.15.9 --dir frontend run lint:check
corepack pnpm@9.15.9 --dir frontend run test:run
corepack pnpm@9.15.9 --dir frontend run build
make -C backend build
```

以上是待执行操作单，不是每一条都已在最终配对版本通过。前端构建写入 `backend/internal/web/dist`；后端产物 `backend/bin/server`。记录制品哈希、Go/Node/pnpm 版本及命令结果。生成物有漂移时先定位，不能将无关生成修改混入发布。

Go 默认、unit 和 integration 是不同门禁；`make test` 还包含 golangci-lint。集成测试必须实际启动隔离 PostgreSQL/Redis，不能把 Docker 不可用导致的 skip 当通过。

## 3. 数据迁移

迁移在服务启动自动执行。不要使用旧 README 中不存在的 `make migrate-up/down`，不要手工插入 schema_migrations 来绕过失败。

| 新迁移 | 内容 |
| --- | --- |
| `239_phone_auth_identity.sql` | phone 身份及唯一性兼容 |
| `240_desktop_model_credentials.sql` | 托管推理凭据关联 |
| `241_desktop_credential_identity_revocation.sql` | 凭据身份及撤销约束 |
| `242_desktop_usage_correlation.sql` | 用量关联与结算字段 |
| `243_desktop_usage_indexes_notx.sql` | 并发查询索引，非事务执行 |
| `244_payment_client_order_id.sql` | 桌面订单持久幂等及恢复字段 |

先对隔离旧数据副本启动新服务，核对迁移 checksum、原用户 ID、余额、订阅、普通 Key、订单和 email/OAuth 登录。备份必须可恢复。已应用的 SQL 不改内容/文件名/编号，不混入 Down SQL；checksum 不符先停发布核对实际已应用版本，不能强行改表中 checksum。

## 4. 短信与注册配置

基础配置模板是 `deploy/config.example.yaml` 的 `sms` 节。行为参数可通过管理员短信设置保存，映射到 `sms.*`；凭据只从服务端部署配置提供，不从管理员响应回显。

| 配置键 | 要求 |
| --- | --- |
| `sms.enabled` | 默认 false；资源核验完成后才开启 |
| `sms.provider` / `sms.region_id` | 当前只支持 aliyun，默认 cn-hangzhou |
| `sms.access_key_id` / `sms.access_key_secret` | 专用服务端凭据；可由 `SMS_ACCESS_KEY_ID` / `SMS_ACCESS_KEY_SECRET` 注入 |
| `sms.hmac_secret` | 至少 32 字节随机秘密；多副本一致，可由 `SMS_HMAC_SECRET` 注入；不放 Git |
| `sms.sign_name` / `sms.template_code` | 控制台核验原文 |
| `sms.template_params` | 键为正式模板变量名；值为 `code` 或 `ttl_minutes`，当前实现要求两种语义均有映射 |
| `sms.template_verified` | 核对模板和有效期后才设 true，不用于跳过验证 |
| `sms.code_length` / `sms.ttl_seconds` | 默认 6 / 300；以实际模板为准 |
| `sms.request_timeout_seconds` / `sms.cooldown_seconds` | 默认 5 / 60；超时保留未知结果与冷却，不自动重发 |
| `sms.max_attempts` | 默认 5 |
| `sms.phone_hour_limit` / `sms.phone_day_limit` | 默认 5 / 10 |
| `sms.ip_hour_limit` / `sms.global_day_limit` | 默认 30 / 1000 |

如果正式模板只有验证码而没有有效期变量，当前配置校验无法满足：先做明确的模板/实现对齐任务，不伪造 `ttl` 变量。公共 phone 能力由 SMS Ready 派生，`registration_enabled` 另行控制；关闭注册不阻止已绑定手机的老用户登录。人机验证启用时桌面同样必须验证，不能以 desktop 标识豁免。正式 API 不提供固定 `1234` 登录。

Redis 必须与生产身份一致地配置并可用；故障时应拒绝发码/校验，不退回进程内限流。不要记录完整手机号、验证码、AccessKey、令牌或响应秘密。

## 5. 内置模型

管理员设置中的 `desktop` 对象映射以下 settings 键，不是新环境变量：

| 键 | 要求 |
| --- | --- |
| `desktop.enabled` | 默认 false |
| `desktop.models` | 默认空；每项引用真实现有 group_id，不另造价格表 |
| `desktop.default_model_id` | 空表示无默认；必须是当前目录条目 |
| `desktop.credential_ttl_seconds` | 默认 3600，校验范围 300–3600 |

目录项字段：`id`、`group_id`、`model`、`display_name`、`provider_label`、`platform`、`api_mode`、`sort_order`、`agent_verified`、`context_window`、`max_output_tokens`、`capabilities`（tools/vision/reasoning）。协议仅 `chat_completions` / `responses` / `anthropic_messages`。未知上下文/输出限制为 null；能力未实测不要标 true，默认模型必须有已验证 Agent 工具/流式支持。

组的 allowlist、用户授权/订阅和原价格服务仍是权威。可用目录不等于扩大用户分组权限。只给测试账户授予测试分组可以限制模型灰度；当前 phone 全局功能没有独立逐用户开关，不能宣称全站开关已经实现精细手机号灰度，先在预发布验收。

账户地址为 `https://api.agentera.com.cn/api/v1`，模型地址为 `https://api.agentera.com.cn/v1`。`desktop_api_version=1` 只是适配版本；Agent 必须实际报告 `managed_model_binding:1`，不能只看应用版本。账户 JWT 和推理 Key 不可混用，普通网站 Key 不得被托管撤销误伤。

## 6. 支付与上线顺序

复用管理员支付配置和原渠道实例。`payment_enabled` 控制支付功能；余额充值的禁用/倍率/费率、可见支付宝/微信方式和商户实例仍使用现有支付配置入口。先核对币种、原价/手续费/到账额度、渠道限额、合法 HTTPS 渠道地址和签名回调，不能直接更新数据库余额。

下表是 `payment_config_service.go` 中实际设置键与管理员 JSON 字段的对应关系，不是新增环境变量；通过原管理员配置入口修改，不手写数据库：

| settings 键 | 管理员 JSON 字段 | 含义 |
| --- | --- | --- |
| `payment_enabled` | `enabled` | 支付总开关 |
| `MIN_RECHARGE_AMOUNT` / `MAX_RECHARGE_AMOUNT` | `min_amount` / `max_amount` | 单笔全局上下限，渠道限额仍另行生效 |
| `DAILY_RECHARGE_LIMIT` | `daily_limit` | 每日充值限制 |
| `ORDER_TIMEOUT_MINUTES` | `order_timeout_minutes` | 订单有效期 |
| `MAX_PENDING_ORDERS` | `max_pending_orders` | 待处理订单数量限制 |
| `ENABLED_PAYMENT_TYPES` | `enabled_payment_types` | 启用渠道类型；还需有效渠道实例/可见方式配置 |
| `BALANCE_PAYMENT_DISABLED` | `balance_disabled` | 单独关闭余额充值 |
| `BALANCE_RECHARGE_MULTIPLIER` | `balance_recharge_multiplier` | 原余额充值倍率政策 |
| `RECHARGE_FEE_RATE` | `recharge_fee_rate` | 充值手续费百分比 |
| `SUBSCRIPTION_USD_TO_CNY_RATE` | `subscription_usd_to_cny_rate` | 原订阅换算配置，不能据字段名推断余额兑换计算 |

桌面报价直接复用服务端原金额计算，不复制汇率或费率。实际支付方式由渠道实例、可见方式配置和限额共同决定，启用一个布尔开关不等于充值已可用。

推荐顺序：可恢复备份 → 新 API/迁移（新开关关闭）→ 配置并完成隔离/预发布验证 → 授权测试账户验证模型/账本和支付 → 配对桌面制品灰度 → 观察并扩大。不可用时保持明确禁用提示，保留余额、订单查询。

桌面下单使用 `amount_decimal`、`client_order_id` 和 `payment_source:aino_desktop`；旧站点 numeric amount 保持兼容。商户已接单但超时必须恢复同一 out_trade_no，不重新创建商户单。付款跳回只刷新查询；PAID/RECHARGING 不等于 COMPLETED，重复正确回调也只能一次入账。

## 7. 回退

1. 先停新注册和新托管凭据签发，必要时关闭新充值入口；保留已付订单查询和签名回调/履约，不为停充值切断已付款入账。
2. 通过受控撤销入口撤销相应设备/父会话托管凭据，确认普通网站 Key、BYOK 不受影响。不要把安装 UUID 当授权秘密。
3. 桌面提示暂不可用或回到已验证的兼容制品；不要删除用户项目、配置或本机历史。
4. API 只回退到已经验证能识别新增 phone 身份和订单字段的兼容版本。尚未提供实际旧 SHA 时，不可填写“回退已验证”。
5. 保留 identity、账本、订单、新列及索引。修复迁移只追加 forward-only 文件，不删数据/改余额/伪造 checksum。需要数据库恢复时按已批准的备份演练流程处理在途付款，不能简单覆盖掉新账本。

## 8. 发布回执必须填写

记录 API/桌面 SHA、镜像/安装包 hash、迁移列表/checksum、配置审核人、环境、授权范围、短信受理与实际送达、模型工具/流式请求及账本差值、订单/商户号与一次性入账、回退目标及演练结果。没有执行的栏保持“未验证”；本文件不授予生产操作权限。
