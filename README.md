# CON 合同状态机与审批工作流

本仓库实现合同状态机、规则审批、模板、草稿、签署文件和跨系统投递的后端核心能力；当前需求导航见[合同核心需求](../sharedDocs/02_需求/拆分需求/03_合同核心与履约.md)。技术栈为 Go、Gin、GORM、MySQL 和 Temporal；HTTP 接口沿用基础平台的 `/api/v1`、统一响应包裹和 HttpOnly Cookie 会话。

2026-10-22 已按当前 UI、客户端、Router/鉴权、Service、Workflow、事务/迁移和装配核实当前操作，未执行新的部署或动态验收。用户步骤、成功判断和失败处理见[合同管理操作手册](../sharedDocs/10_操作手册/合同管理操作手册.md)，逐文档纠正与源码依据见[核验台账](../.ai/modules/operation-docs-business-20261013.md)。需求和历史资料不因此成为上线事实。

## 已实现能力

- 9 状态合同状态机：`draft → pending → approved → active → in_progress → pending_pay → completed → archived`，并支持履约阶段终止后归档。
- 普通状态直接流转；执行中、待付款、终止、归档走管理员状态变更审批。
- 合同提交后的多级审批。默认节点为销售总监、技术总监、财务总监；审批规则可按金额、服务类型、客户信用、合同类型和条款一致性组合 AND/OR 条件，并按优先级选择唯一规则。
- 全部审批节点通过后，系统向 `contract_specialist` 角色投递待签署通知，合同专员在签署台账中实时接收已生效合同。
- Temporal 长流程：72 小时节点超时、定时提醒、通过、拒绝、会签/或签、转交、退回历史节点、申请人撤回、评论和催办。
- 提交时固化规则版本、节点和合同 SHA-256；运行中规则变更不影响既有流程，审批完成前合同内容发生变化会阻断生效。
- MySQL 事务保存合同状态、生命周期事件、审批实例、任务和动作；Activity、通知 outbox 均有幂等键，可安全重试。
- 合同激活事务同步写入项目投递 Outbox；API/Worker 通过受控内部网络异步投递，失败指数退避重试，项目侧按稳定合同ID唯一建项（历史无ID回退合同号），行版本仅用于并发/审计/展示，不释放重复建项。项目迁移000033对存量同合同多项目失败关闭。
- 草稿可维护结构化付款计划；审批生效事务同时写项目投递及合法的 `contract.financial_effective.v1` 财务事件。财务 Dispatcher 启用并取得结算认可的机器身份后投递；结算消费后生成待确认应收计划。旧无分期合同、签署完成、履约 completed 和正文模板付款变量均不会合成默认分期。
- Worker 启动时确保每日自动归档 Cron Workflow 存在；默认北京时间零点执行，通知合同负责人、销售总监角色和管理员角色。
- 使用 `github.com/coreos/go-oidc/v3` 对接 Keycloak OIDC Authorization Code + PKCE：通过 discovery 获取授权、Token、UserInfo、JWKS 和登出端点，独立校验 `state`、`nonce`、ID Token 签名/Issuer/Audience/有效期，并校验 Keycloak 映射的 `tenant_id`、`roles`、`permissions`、`role_config_hash`、`authz_revision` 后建立仅作用于子路径的 `contract_management_session`。
- 本地会话通过轮换 Refresh Token 定期取得最新授权快照；角色撤销、权限变更和授权版本更新默认在一分钟内生效。
- 启用 `PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED` 后，API 启动时使用独立机器 Client 将内嵌权限清单发布到平台；同步失败时拒绝启动，避免运行时权限与平台目录漂移。
- 配置 `PLATFORM_AUDIT_CLIENT_ID`、`PLATFORM_AUDIT_CLIENT_SECRET`、`PLATFORM_APPLICATION_CODE` 和 `PLATFORM_ENVIRONMENT_CODE` 后，合同写操作会以 OAuth Client Credentials 和 `audit.ingest` scope 写入基础平台审计。
- 合同服务自身提供 `/`、`/auth/login`、`/auth/callback`、`/auth/logout` 和合同台账页面；统一门户通过 `/contract_management/` 访问，业务浏览器经统一网关访问；当前生产Compose另为contract-api发布127.0.0.1:18081维护端口（可由CONTRACT_API_PORT覆盖），数据库和Temporal不对外网发布。
- 超级管理员或具备 `contract.template.manage` 权限的用户可上传不超过 10MB 的 DOCX 合同模板；销售人员在新建合同时选择模板、填写动态字段、预览并导出渲染后的 DOCX。渲染结果随合同固化，不受后续模板变化影响。

## 目录

```text
cmd/api                         HTTP 服务
cmd/worker                      Temporal Worker
internal/domain/contract        状态机和合同模型
internal/domain/approval        审批模型与规则表达式引擎
internal/workflows              Temporal Workflows / Activities
internal/application            用例、权限和工作流启动/Signal
internal/infrastructure/mysql   GORM 模型、事务存储与 outbox
internal/infrastructure/platform OIDC、独立会话与平台审计
internal/transport/httpapi      Gin REST API
authz/permission-manifest.yaml  版本化权限、角色、数据范围和平台兼容性清单
migrations                      MySQL DDL
```

权限清单以 `authz/permission-manifest.yaml` 为合同后端权限语义的版本化来源。合同 API 会把清单转换为平台通用授权目录并通过 `authorization.catalog.sync` 发布；前端菜单权限不能替代本清单标记的后端执行点。权限码仅精确匹配，`all` 等通配权限会被拒绝。

合同按可信租户及平台下发的 SELF、组织、项目、ALL 数据范围执行过滤；仅销售角色的contract.read/contract.edit（含状态变更）统一收紧为本人负责合同。管理员/总监仍须实际权限与范围，不凭角色名自动拥有全部合同。审批详情还检查 contract.read 范围；任务身份不等于台账全量访问权。草稿编辑/提交另检查 admin 或草稿创建销售，不能用负责人身份替代创建人。

Temporal Workflow 只做确定性编排，时间使用 `workflow.Now/NewTimer`；数据库、通知和审计事实都由可重试 Activity 完成。持久化统一使用 GORM，悲观锁使用 `clause.Locking`，幂等写入使用 `clause.OnConflict`；生产表结构仍以显式 SQL 迁移为准，不在服务启动时执行 `AutoMigrate`。

## 状态流转

```text
draft -> pending -> approved -> active -> in_progress -> pending_pay -> completed -> archived
                                      \             \               \
                                       +-------------+----> terminated -> archived
```

系统内部在同一事务完成 `pending → approved → active`。项目补充协议接受 approval_passed=true 且 approved/active，仍要求不同合同、同客户与权威ID一致，不接受 draft/pending/in_progress/terminated/archived。归档为终态，不提供归档恢复重审或直接更换盖章文件。`in_progress`、`pending_pay`、`terminated`、`archived` 必须启动 `contract-status-change` Workflow；`completed` 等非关键目标可在状态机校验和乐观锁保护下直接变更。

## HTTP API

所有业务接口都需要合同系统自己的 OIDC 本地会话；成功/失败响应遵循 `{code,message,request_id,data}`。

| 方法 | 路径 | 权限 | 行为 |
|---|---|---|---|
| GET | `/api/v1/auth/me` | 已登录 | 返回平台授权快照，用于前端菜单和按钮展示 |
| POST | `/api/v1/contracts` | `contract.create` | 创建草稿并计算正文 SHA-256 |
| GET | `/api/v1/contract-templates` | `contract.create` 或 `contract.template.manage`（兼容 `admin` 角色） | 查询当前租户可用的 DOCX 模板和动态字段 |
| GET | `/api/v1/dashboard` | `contract.read` | 当前有效数据范围的总額、总数、审批中、有效/超期汇总及最近 200 条明细 |
| POST | `/api/v1/contract-templates` | `contract.template.manage`（兼容 `admin` 角色） | 上传并解析 DOCX 模板，最大 10MB |
| PUT | `/api/v1/contract-templates/{id}` | `contract.template.manage`（兼容 `admin` 角色） | 编辑模板名称、字段标签、默认值和管理员锁定状态 |
| DELETE | `/api/v1/contract-templates/{id}` | `contract.template.manage`（兼容 `admin` 角色） | 删除模板；已生成合同保存的是冻结文档，不受影响 |
| POST | `/api/v1/contract-templates/{id}/preview` | `contract.create` | 根据表单值渲染安全的 HTML 预览 |
| GET | `/api/v1/contracts/{id}` | `contract.read` | 查询实际授权范围内合同及服务端 can_edit_draft |
| PUT | `/api/v1/contracts/{id}/draft` | `contract.edit` | 当前范围内 admin 或草稿创建销售按 expected_version 保存 |
| GET | `/api/v1/contracts/{id}/lifecycle` | `contract.read` | 查询实际授权范围内合同生命周期 |
| GET | `/api/v1/contracts/{id}/preview` | `contract.read` | 按 DOCX 段落、字体和表格结构预览已固化的合同文档 |
| GET | `/api/v1/contracts/{id}/export` | `contract.read` | 导出实际授权范围内固化的 DOCX |
| GET | `/api/v1/approved-contracts` | `contract.approved.read` | 当前实际范围内已批准及后续状态合同 |
| GET | `/api/v1/approved-contracts/{id}/docx` | `contract.document.download` | 下载审批时冻结的 DOCX |
| GET | `/api/v1/approved-contracts/{id}/pdf` | `contract.document.download` | 使用 LibreOffice 转换并下载格式化 PDF |
| PUT | `/api/v1/approved-contracts/{id}/stamped-pdf` | `contract.stamped_pdf.upload` | 上传不超过 20MB 的盖章合同 PDF |
| POST | `/api/v1/contracts/external` | `contract.create` | 通过 multipart `file` + `metadata` 上传不超过 10MB 的外部 DOCX 合同草稿 |
| POST | `/api/v1/contracts/{id}/submit-approval` | `contract.create` | 匹配规则并启动合同审批 |
| POST | `/api/v1/contracts/{id}/status-changes` | `contract.edit` | 直接流转或启动关键状态审批 |
| GET | `/api/v1/approvals` | 已登录 | 当前用户发起的审批与历史状态 |
| GET | `/api/v1/approvals/tasks` | `approval.process` | 当前用户待办 |
| GET | `/api/v1/approvals/{id}` | `approval.view`、`approval.process` 或申请人 | 聚合查询合同审批内容、规则快照元数据、Temporal 当前流程状态和处理记录 |
| POST | `/api/v1/approvals/{id}/approve` | `approval.process` | 同意 |
| POST | `/api/v1/approvals/{id}/reject` | `approval.process` | 拒绝，意见必填 |
| POST | `/api/v1/approvals/{id}/sign` | `approval.process` | 加签；传 `target_user_ids`、`countersign=all/any` |
| POST | `/api/v1/approvals/{id}/transfer` | `approval.process` | 转交；目标只能有一人 |
| POST | `/api/v1/approvals/{id}/return` | `approval.process` | 退回已通过节点；传 `target_node_id` |
| POST | `/api/v1/approvals/{id}/withdraw` | 申请人 | 审批完成前撤回 |
| POST | `/api/v1/approvals/{id}/urge` | 申请人或 `approval.manage` | 催办并写 outbox |
| POST | `/api/v1/approvals/{id}/comments` | `approval.view` | 评论记录 |
| GET/POST | `/api/v1/approval-rules` | `approval.view` / `approval_rule.manage` | 查询或新增审批规则 |
| PUT/DELETE | `/api/v1/approval-rules/{id}` | `approval_rule.manage` | 按 `version` 乐观锁更新或删除规则 |

动作接口返回 `202` 表示 Signal 已由 Temporal 接收，并在响应中返回唯一 `command_id`。客户端必须在审批详情的动作记录中观察到相同 `command_id` 后才能提示处理成功；终态操作还需等待合同状态完成同步。审批中心在页面可见时按秒同步发起列表、待办和已打开详情，持久化待办和动作记录仍由 Activity 事务性更新。

DOCX 模板变量可直接使用中文字段，例如 `{{客户名称}}`；也兼容 `{{field_name:中文字段名}}`、带默认值的 `{{发票类型 '专票'}}` 和原型模板中的 `{{金额_大写 合同金额}}`。变量可位于正文、页眉或页脚，并允许被 Word 拆分为多个文本片段。创建合同请求通过 `template_id` 和 `template_values` 提交字段值，服务端会重新渲染，不能用客户端预览内容替代。

### 收入合同模板导入

`收入合同模版/` 的五份 DOCX 可直接在“合同模板 → 上传模板”中选择上传，无须改写正文或转换文件。上传后可编辑字段显示名称、默认值、管理员锁定值与合同编号格式。付款比例、税率、银行账号等占位符只是合同正文变量，不自动创建付款计划或结算业务字段。

也可从本仓库运行批量工具，默认只做本地校验，不访问网络：

```bash
go run ./cmd/import-templates -dir ./收入合同模版
```

实际导入必须使用已有合同管理登录会话，指定预期租户 ID，并显式启用 `-apply`。`-cookie-file` 指向权限为 0600 的私有文件，仅保存现有合同会话的 Cookie 请求头值；不要把凭据写入命令行、仓库或日志。

```bash
go run ./cmd/import-templates -dir ./收入合同模版 \
  -url http://localhost:8081/contract_management \
  -tenant YOUR_TENANT_ID -cookie-file /absolute/private/session-cookie -apply
```

工具先验证所有文件，再检查当前用户的租户和模板管理权限，通过原有审计/同源校验/文件网关上传接口逐项导入。同名模板报告 SKIP，不比较正文、不覆盖已有配置；失败立即停止，之前成功的导入保留，重跑可跳过同名项。不要同时运行多个导入进程。工具不会绕过登录、直接写数据库或自动替所有租户安装模板。

## 本地运行

要求 Go 1.25.4+、MySQL 8.4+ 和 Temporal Server。复制配置后启动基础依赖：

```bash
cp .env.example .env.local
# 填入 Keycloak Client Secret、Tenant ID 和在 Keycloak 登记的精确回调地址。
docker compose --env-file .env.local up -d --build
```

Compose 会先等待 MySQL 健康，再由一次性 `migrate` 服务按编号执行所有待完成迁移；只有迁移成功后 API 才会启动。API 默认监听 `:8081`，但 Compose 只通过平台 Docker 网络暴露；门户网关仅把 `/contract_management/api/`、`/contract_management/auth/` 等后端路径转发至本服务并去除前缀，其余 `/contract_management/` 页面由统一前端承载。

启用合同到项目对接时，设置 `PROJECT_INTEGRATION_ENABLED=true` 和 `PROJECT_API_BASE_URL`，项目服务设置 `CONTRACT_INTEGRATION_ENABLED=true`。Compose 内项目地址为 `http://project-api:8082`，内部接口不得通过公网或门户网关暴露。投递达到 `PROJECT_INTEGRATION_MAX_ATTEMPTS` 后进入 `dead` 状态，需运维核对网络和项目接口后重新置为 `pending`。

外部合同创建必须启用 CRM 权威复核：设置 `CRM_REFERENCE_ENABLED=true`、CRM 内网地址、平台 Token 地址和独立 `client_credentials` 凭据。该客户端只能授予 `customer.contract_reference.read`；每次查询都携带时间戳与随机 nonce，CRM 会校验租户、活动客户状态以及可选商机的客户归属。目录不可用时创建失败关闭，不会回退为信任浏览器快照。

审批人根据基础平台中合同应用的有效角色动态解析，直接用户授权、组织授权和岗位继承均会生效。同一角色有多人时采用或签，任一人处理后进入下一节点。不要在镜像或仓库中保存 Temporal API Key 或数据库密码。

审批节点角色编码必须与权限清单一致：`admin`、`sales_director`、`tech_director`、`finance_director`。同一合同版本最多只能存在一个运行中的关键状态变更审批。

OIDC 浏览器 Client 与审计机器 Client 必须分离。Issuer 必须是实际接入环境的 realm 地址，不能只填写 Keycloak 根地址；旧示例主机不能作为当前部署事实。Keycloak Client 必须配置精确回调地址，并将 `tenant_id`、`roles`、`permissions`、`role_config_hash`、`authz_revision` 映射到 ID Token 和 UserInfo。审计凭据必须由密钥管理系统注入；未完整配置审计的四项环境变量时，审计投递保持禁用。

平台管理员应通过“子系统一键接入”注册 `contract_management/dev`，公开 BaseURL 使用门户地址，UpstreamURL 使用 `http://contract-api:8081`，路径前缀使用 `/contract_management`。首次启用审计时，应另行创建 `service + client_secret_basic + client_credentials` 客户端并授予 `audit.ingest` scope。

Temporal Cloud 可设置 `TEMPORAL_TLS=true`、`TEMPORAL_API_KEY`、命名空间和地址。API Key 会通过 SDK Credentials 注入，不写入 Workflow history。

## 验证

以下为工程已有验证入口参考，本次仅修改文档，没有执行这些命令。

```bash
make fmt
make test
make vet
make build
```

领域测试覆盖合法/非法状态流转与规则优先级；Temporal 测试覆盖三级通过、撤回和关键状态审批通过。数据库集成测试应在 CI 中对迁移后的 MySQL 执行。

## 自动部署

当前[CI/CD 工作流](.github/workflows/ci-cd.yml)在 main 分支通过测试后构建 linux/amd64 镜像，推送配置的 ACR，再按不可变摘要调用远端部署脚本，不是 GHCR 或固定主机。部署任务使用 GitHub test Environment；源码配置不证明真实 Environment、镜像发布或部署已完成。仓库需要配置：

- 镜像变量 `ACR_PUSH_REGISTRY`、`ACR_PULL_REGISTRY`、`ACR_NAMESPACE`、`ACR_REPOSITORY`，凭据 `ACR_USERNAME`、`ACR_PASSWORD`。
- Environment Secret `DEPLOY_HOST`：实际受控部署主机。
- Environment Secret `DEPLOY_USER`：服务器上的低权限发布用户。
- Environment Secret `DEPLOY_SSH_KEY`：该用户的 Ed25519 SSH 私钥。
- Environment Secret `DEPLOY_KNOWN_HOSTS`：预先核验的实际部署主机公钥，不能在流水线中临时信任。
- 可选 Environment Secret `DEPLOY_PORT`：SSH 端口，默认 `22`。
- 可选 Environment Variable `DEPLOY_PATH`：集成部署目录，默认 `/opt/unified-identity-platform`。

服务器部署目录必须已完成平台受控初始化，具有可执行的 bin/deploy-service.sh 和对应运行配置；发布账号具备必要 Compose 权限及 ACR 拉取能力。流水线传递 ACR 镜像摘要，并设置 DEPLOY_FAIL_IF_RUNTIME_NOT_READY=true。实际备份、迁移、健康检查与回滚由平台脚本及运行环境决定，本次未执行或验证远端操作。

首次配置主机公钥时，必须通过可信渠道核验实际主机指纹后保存 Secret；单纯 ssh-keyscan 输出不能替代指纹核验。

test Environment 的审批门禁按实际环境治理策略配置，不为自动部署而关闭必要审批。

仓库提供了 Environment 配置模板和自动配置脚本。安装并登录 GitHub CLI 后执行：

```bash
cp .github/environments/test.env.example .github/environments/test.env
# 编辑 test.env，填入发布用户和本机 SSH 文件绝对路径。
set -a
source .github/environments/test.env
set +a
./scripts/configure-test-environment.sh
```

模板只允许保存密钥文件路径，不能保存私钥内容。辅助脚本不替代上文当前 CI/CD 所需完整 ACR 与 DEPLOY_HOST 配置；运行前应核对脚本与环境治理要求，本次未执行。

## 生产注意事项

### 收入模板修正版

原始 `收入合同模版/` 文件保留；运行 `scripts/repair_income_templates.py` 生成
`收入合同模版/修正版/` 五份受控字段修正版。在部署新后端和前端后，管理员在
“合同模板”卡片使用“替换模板文件”，上传对应修正版，保留模板ID、编号规则及同名字段设置。
替换接口是 `PUT /api/v1/contract-templates/:templateID/source`，multipart字段为 `file`；
仍要求模板管理权限、当前租户及同源请求。已有合同的正文快照不会被模板替换重写。

`合同金额` 在保存时使用合同台账金额；中文大写由 `金额_大写` 模板辅助函数生成。
`合同编号`/`系统合同编号` 是系统保留字段，不接受人工正式编号；草稿显示审批后生成提示，
审批完成时在同一事务内写入正式编号、冻结DOCX及其哈希，不重新读取可变模板。
付款金额、联系人和地点这些正文模板变量仍不自动生成财务分期。付款计划需在新建/编辑草稿的独立区域维护，并随同一合同版本审批冻结。

### 付款计划与财务事件

模板创建、外部 DOCX 创建和草稿 `PUT /api/v1/contracts/:contractID/draft` 共用 `payment_terms`：每期包含 `installment_no`、`due_date`（有效 `YYYY-MM-DD`）和十进制字符串 `amount`。最多 100 期，分期号不重复，金额为正数且最多两位小数，合计必须精确等于 `amount_minor`；所有分期使用同一合同币种。关联 CRM 客户及可选商机经当前用户的 CRM 只读范围复核。

分期写入与合同创建/草稿 CAS 在同一事务内完成。更新仍要求 `expected_version`、原有编辑权限和创建人/数据范围；进入审批后及生效后不能原地修改。旧客户端省略 `payment_terms` 时保留已有计划，金额变化仍需重新核对总和；币种变化必须显式提供计划。显式空数组仅能在草稿中清除计划。原外部 DOCX 不随金额编辑改写；其金额/币种冻结规则继续生效。

查询和 completed-contracts 摘要返回已保存的真实分期，按原合同读取权限及租户范围过滤。旧无分期合同保留空数组，不补造计划。财务出站使用 `SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED`、`SETTLEMENT_API_BASE_URL` 及独立机器 OAuth 配置；默认关闭，受控部署应下发具有 `settlement.contract.ingest` 的专用身份。分期和出站配置都齐备后才构成完整结算投递链。
准确的模块开发/技术咨询交付分类依赖项目管理 migration27 和租户启用目录，部署后需再次验收。

- 通知采用 transactional outbox；API 装配已有受配置控制的 Dispatcher，异步投递平台通知并记录结果。pending 不代表送达，不能仅凭待签署通知记录宣称客户或专员已收到消息。
- API 与 Worker 必须使用相同的 Temporal namespace/task queue 和兼容的 Workflow 代码。上线修改 Workflow 时保留 replay 测试历史；不要直接改动已经执行过的确定性分支。
- `MYSQL_DSN` 必须包含 `parseTime=true`；数据库账号仅授予 `contract_management` 所需权限。
- 审批命令的权限先由 HTTP 用例校验，Workflow 再校验当前处理人/申请人，形成双重业务约束。
# 审批人员目录

## 草稿返回编辑

合同详情的“返回上一步编辑”复用新建表单回填，只允许当前租户授权范围内的 admin，
或编辑自己创建草稿的销售。负责人不是创建人的替代判断；还必须持有 `contract.edit`。
保存接口为 `PUT /api/v1/contracts/:contractID/draft`，传完整已有结构字段与必填
`expected_version`。保留合同ID、创建人、负责人、数据范围及模板来源，成功后版本递增并记录编辑审计。
非草稿、跨范围、其他销售的草稿及过期版本失败关闭，不可通过直接调用接口绕过。

“保存并发起审批”先保存，然后用返回的新版本提交现有审批接口；审批失败不撤销成功的草稿保存。
模板合同重新生成正文/哈希；外部合同保留原DOCX，金额及币种不能单独变更，金融条款变化应使用
正确的新DOCX新建外部草稿。GET及列表中的 `can_edit_draft` 是服务端计算能力，不是客户端授权输入。

数据库回归只接受显式 `CONTRACT_TEST_DSN`：loopback TCP，数据库名 `contract_test_*`，
不从业务运行配置读取。专用库预创建后执行
`go test ./internal/infrastructure/mysql -run 'TestDraft(Persistence|EditApprovalRace)E2E' -count=1 -v`。
未配置时不连接数据库。测试迁移仅专用库，并清理自己的随机测试租户。

## 审批目录权限

合同审批人不从 Keycloak Token 中读取业务角色。API 使用独立的基础平台机器客户端调用
`/api/v1/internal/owner-directory`，按审批角色实时筛选可选人员。该客户端只能授予
`owner_directory.read`，通过 `PLATFORM_PERSONNEL_DIRECTORY_CLIENT_ID` 和
`PLATFORM_PERSONNEL_DIRECTORY_CLIENT_SECRET` 注入；缺失、无权限、平台不可用或响应不完整时
审批规则失败关闭，不回退为所有用户。

## 统计、签署与文件操作边界

仪表盘汇总按范围查询，状态/类型分布与报表明细最多最近更新 200 条；有效计数包含 archived，超期总数为 active/in_progress/pending_pay 且到期日已过。金额按currency_totals分币种汇总，无汇率换算，不跨币种相加。

签署台账独立记录 pending_shipment、in_return、pending_review、completed；核验不改变履约状态。催办仅保存次数/时间，不发客户短信。盖章 PDF 最大20MiB，按合同与文档版本形成不可变链；确认须绑定document_version + content_sha256。已确认文件更换需当前版本和原因并重新核验；归档合同拒绝更换。required 网关上传失败前旧 BLOB 可能已入库，应查详情后再重试，历史缺 READY 文件记录也不证明已全面迁移网关。

当前 Vue API 未提供通用状态变更入口；后端支持不等于已有按钮。实际寄出、签收、上传、核验及失败处理见操作手册。

## 模板源文件生命周期

迁移 `000030_template_file_lifecycle.sql` 为每个模板源建立不可变代次及持久队列，并登记当前已有的网关文件；`000031_template_file_upload_attempt.sql` 保存上传尝试计数和失败文件历史。新建/替换先在同一 MySQL 事务提交模板 BLOB、版本和 `PENDING` 投递意图；版本冲突或数据库失败不会先上传、绑定文件。即使 `required` 模式，接口成功也只代表模板和投递意图已持久化，远端完成状态以 `file_gateway_state=READY` 为准。本地模板渲染仍使用已保存的正文。

替换、删除在业务事务内退休旧代次；模板行删除后，源代次与释放意图仍保留。新代次使用独立 `contract_template_source + source_id + TEMPLATE_SOURCE` 绑定身份，旧代次只释放自己的绑定；迁移登记的历史来源沿用 `contract_template + template_id + TEMPLATE_SOURCE`。释放调用平台 `POST /api/v1/files/{file_id}/bindings/release`，只停用业务引用，保留文件字节、LONG_TERM 保留类别及源文件证据。平台必须保留已释放 tuple 的墓碑，拒绝超时请求的迟到绑定。

内嵌模式由 API、拆分模式由 `contract-worker` 消费队列；`file-backfill` 的模板部分也使用同一队列。上传结果未知时先通过 `GET /api/v1/file-upload-requests/{request_id}` 解析原请求：WRITING 只等待，READY 复用稳定 fileID；只有确认 FAILED 且旧引用释放成功后，才原子保存失败 fileID/尝试号并分配下一次上传 key。REJECTED 停止自动重试，保留失败状态，需替换源文件修正内容。绑定/释放失败保留记录并退避重试，最大间隔 512 秒，数据库只保存固定错误码。

退休代次只执行 `POST /api/v1/file-upload-requests/{request_id}/retire` 后精确释放引用，不上传文件。请求尚未预留时网关持久化 CANCELLED 墓碑，阻止迟到 Reserve；已有 WRITING/READY 等请求返回稳定 fileID，随后释放墓碑阻止迟到 Bind。退休清理优先消费，并不受商业授权过期阻断；新增上传和绑定仍按原授权门禁执行。投递期间持有模板/代次行锁，网关调用总预算 30 秒，数据库处理总预算 40 秒，因此同模板的替换/删除可能短暂等待。

启用网关时机器授权及 `FILE_GATEWAY_SCOPE` 需包含 `platform:file:upload platform:file:bind`。消费者以 `OIDC_TENANT_ID` 在数据库筛选并核对源租户后才外发；模板回填 CLI 同样要求该租户配置并按租户筛选。发布须先按既有备份/迁移门禁应用迁移，并同步上线平台网关 `000104` 精确引用释放和 `000105` 上传请求退休能力；队列未清空时应保持消费者和对应凭据可用。迁移只能登记当前业务行已知来源，历史失败留下且本地已丢失身份的孤立绑定需另行只读盘点和受控处理。

隔离数据库验证：显式配置既有规则允许的 `CONTRACT_TEST_DSN` 后，运行 `go test ./internal/infrastructure/mysql -run TestTemplateFile -count=1 -v`。未配置时这些用例明确跳过，不连接业务库。
