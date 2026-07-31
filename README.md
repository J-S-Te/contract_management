# CON 合同状态机与审批工作流

本仓库实现 `原始需求.md` 中 CON-002、APP-001、APP-002、APP-004 和 APP-005 的后端核心能力。技术栈为 Go、Gin、GORM、MySQL 和 Temporal；HTTP 接口沿用基础平台的 `/api/v1`、统一响应包裹和 HttpOnly Cookie 会话。

## 已实现能力

- 9 状态合同状态机：`draft → pending → approved → active → in_progress → pending_pay → completed → archived`，并支持履约阶段终止后归档。
- 普通状态直接流转；执行中、待付款、终止、归档走管理员状态变更审批。
- 合同提交后的多级审批。默认节点为销售总监、技术总监、财务总监；审批规则可按金额、服务类型、客户信用、合同类型和条款一致性组合 AND/OR 条件，并按优先级选择唯一规则。
- Temporal 长流程：72 小时节点超时、定时提醒、通过、拒绝、会签/或签、转交、退回历史节点、申请人撤回、评论和催办。
- 提交时固化规则版本、节点和合同 SHA-256；运行中规则变更不影响既有流程，审批完成前合同内容发生变化会阻断生效。
- MySQL 事务保存合同状态、生命周期事件、审批实例、任务和动作；Activity、通知 outbox 均有幂等键，可安全重试。
- Worker 启动时确保每日自动归档 Cron Workflow 存在；默认北京时间零点执行，通知合同负责人、销售总监角色和管理员角色。
- 按平台接入规范实现 OIDC Authorization Code + PKCE：独立校验 `state`、`nonce`、ID Token 签名/Issuer/Audience/有效期，并校验平台签发的 `tenant_id`、`roles`、`permissions`、`role_config_hash`、`authz_revision` 后建立仅作用于子路径的 `contract_management_session`，不共享平台 `bp_session`。
- 本地会话通过轮换 Refresh Token 定期取得最新授权快照；角色撤销、权限变更和授权版本更新默认在一分钟内生效。
- 启用 `PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED` 后，API 启动时使用独立机器 Client 将内嵌权限清单发布到平台；同步失败时拒绝启动，避免运行时权限与平台目录漂移。
- 配置 `PLATFORM_AUDIT_CLIENT_ID`、`PLATFORM_AUDIT_CLIENT_SECRET`、`PLATFORM_APPLICATION_CODE` 和 `PLATFORM_ENVIRONMENT_CODE` 后，合同写操作会以 OAuth Client Credentials 和 `audit.ingest` scope 写入基础平台审计。
- 合同服务自身提供 `/`、`/auth/login`、`/auth/callback`、`/auth/logout` 和合同台账页面；统一门户通过 `/contract_management/` 访问，合同 API、数据库和 Temporal 不直接暴露宿主机端口。
- 超级管理员可上传不超过 10MB 的 DOCX 合同模板；销售人员在新建合同时选择模板、填写动态字段、预览并导出渲染后的 DOCX。渲染结果随合同固化，不受后续模板变化影响。

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

合同数据范围固定为负责人本人：任何角色都不能查询、读取、提交或变更非本人负责的合同，系统不定义管理全部合同的数据权限。审批人通过审批任务处理流程，不因此获得合同台账的全量访问权。

Temporal Workflow 只做确定性编排，时间使用 `workflow.Now/NewTimer`；数据库、通知和审计事实都由可重试 Activity 完成。持久化统一使用 GORM，悲观锁使用 `clause.Locking`，幂等写入使用 `clause.OnConflict`；生产表结构仍以显式 SQL 迁移为准，不在服务启动时执行 `AutoMigrate`。

## 状态流转

```text
draft -> pending -> approved -> active -> in_progress -> pending_pay -> completed -> archived
                                      \             \               \
                                       +-------------+----> terminated -> archived
```

系统内部自动步骤为 `pending → approved → active`。`in_progress`、`pending_pay`、`terminated`、`archived` 必须启动 `contract-status-change` Workflow；`completed` 等非关键目标可在状态机校验和乐观锁保护下直接变更。

## HTTP API

所有业务接口都需要合同系统自己的 OIDC 本地会话；成功/失败响应遵循 `{code,message,request_id,data}`。

| 方法 | 路径 | 权限 | 行为 |
|---|---|---|---|
| GET | `/api/v1/auth/me` | 已登录 | 返回平台授权快照，用于前端菜单和按钮展示 |
| POST | `/api/v1/contracts` | `contract.create` | 创建草稿并计算正文 SHA-256 |
| GET | `/api/v1/contract-templates` | `contract.create` 或 `admin` 角色 | 查询当前租户可用的 DOCX 模板和动态字段 |
| POST | `/api/v1/contract-templates` | `admin` 角色 | 上传并解析 DOCX 模板，最大 10MB |
| POST | `/api/v1/contract-templates/{id}/preview` | `contract.create` | 根据表单值渲染安全的 HTML 预览 |
| GET | `/api/v1/contracts/{id}` | `contract.read` | 查询合同；任何角色都只能读取自己负责的合同 |
| GET | `/api/v1/contracts/{id}/preview` | `contract.read` | 按 DOCX 段落、字体和表格结构预览已固化的合同文档 |
| GET | `/api/v1/contracts/{id}/export` | `contract.read` | 导出创建时固化的 DOCX，仅合同负责人可访问 |
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

动作接口返回 `202` 表示 Signal 已由 Temporal 接收。最终状态通过审批详情查询；持久化待办和动作记录由 Activity 最终一致地更新。

DOCX 模板变量可直接使用中文字段，例如 `{{客户名称}}`；也兼容 `{{field_name:中文字段名}}`、带默认值的 `{{发票类型 '专票'}}` 和原型模板中的 `{{金额_大写 合同金额}}`。变量可位于正文、页眉或页脚，并允许被 Word 拆分为多个文本片段。创建合同请求通过 `template_id` 和 `template_values` 提交字段值，服务端会重新渲染，不能用客户端预览内容替代。

## 本地运行

要求 Go 1.25.4+、MySQL 8.4+ 和 Temporal Server。复制配置后启动基础依赖：

```bash
cp .env.example .env.local
# 填入平台“一键接入”返回的 Client ID、一次性 Secret、Tenant ID 和精确回调地址。
docker compose --env-file .env.local up -d --build
go run ./cmd/worker
go run ./cmd/api
```

MySQL 初始化会按编号执行 `migrations` 中的建表和增量约束脚本。API 默认监听 `:8081`，但 Compose 只通过平台 Docker 网络暴露；门户网关仅把 `/contract_management/api/`、`/contract_management/auth/` 等后端路径转发至本服务并去除前缀，其余 `/contract_management/` 页面由统一前端承载。

审批人根据基础平台中合同应用的有效角色动态解析，直接用户授权、组织授权和岗位继承均会生效。同一角色有多人时采用或签，任一人处理后进入下一节点。不要在镜像或仓库中保存 Temporal API Key 或数据库密码。

审批节点角色编码必须与权限清单一致：`admin`、`sales_director`、`tech_director`、`finance_director`。同一合同版本最多只能存在一个运行中的关键状态变更审批。

OIDC 浏览器 Client 与审计机器 Client 必须分离。浏览器 OIDC 配置来自平台“一键接入”，审计凭据必须由密钥管理系统注入；未完整配置审计的四项环境变量时，审计投递保持禁用。通知 outbox 不会直接调用平台通知控制面，因为当前机器 Token 的已发布权限边界仅包含审计写入。

平台管理员应通过“子系统一键接入”注册 `contract_management/dev`，公开 BaseURL 使用门户地址，UpstreamURL 使用 `http://contract-api:8081`，路径前缀使用 `/contract_management`。首次启用审计时，应另行创建 `service + client_secret_basic + client_credentials` 客户端并授予 `audit.ingest` scope。

Temporal Cloud 可设置 `TEMPORAL_TLS=true`、`TEMPORAL_API_KEY`、命名空间和地址。API Key 会通过 SDK Credentials 注入，不写入 Workflow history。

## 验证

```bash
make fmt
make test
make vet
make build
```

领域测试覆盖合法/非法状态流转与规则优先级；Temporal 测试覆盖三级通过、撤回和关键状态审批通过。数据库集成测试应在 CI 中对迁移后的 MySQL 执行。

## 自动部署

`main` 分支推送通过测试后，GitHub Actions 会构建 `linux/amd64` 镜像，推送到 GHCR，并使用镜像摘要自动部署到 `47.111.20.119`。部署任务固定使用 GitHub `test` Environment；仓库需要配置：

- Environment Secret `DEPLOY_USER`：服务器上的低权限发布用户。
- Environment Secret `DEPLOY_SSH_KEY`：该用户的 Ed25519 SSH 私钥。
- Environment Secret `DEPLOY_KNOWN_HOSTS`：预先核验的 `47.111.20.119` SSH 主机公钥，不能在流水线中临时信任。
- 可选 Environment Secret `DEPLOY_PORT`：SSH 端口，默认 `22`。
- 可选 Environment Variable `DEPLOY_PATH`：集成部署目录，默认 `/opt/basic-platform`。

服务器部署目录必须已经由平台生产部署初始化，包含可执行的 `bin/deploy-service.sh`、`compose.yaml`、权限为 `600` 的 `.env` 和 `.release.env`。发布用户必须能够在该目录运行 Docker Compose；如果 GHCR 包为私有包，服务器还必须预先执行 `docker login ghcr.io`。流水线传递 `ghcr.io/...@sha256:...` 不可变镜像引用，远端脚本负责数据库备份、迁移、服务更新、健康检查和失败时恢复上一镜像。

首次配置主机公钥时，应在可信网络中核验服务器指纹后生成 Secret，例如：

```bash
ssh-keyscan -H -p 22 47.111.20.119
```

为保持完全自动部署，`test` Environment 不应配置必需人工审批；如测试环境治理要求审批，可添加 Required reviewers，此时构建仍自动执行，但部署会等待批准。

仓库提供了 Environment 配置模板和自动配置脚本。安装并登录 GitHub CLI 后执行：

```bash
cp .github/environments/test.env.example .github/environments/test.env
# 编辑 test.env，填入发布用户和本机 SSH 文件绝对路径。
set -a
source .github/environments/test.env
set +a
./scripts/configure-test-environment.sh
```

脚本会创建 GitHub `test` Environment，上传四个 Environment Secrets，并设置 `DEPLOY_PATH` Environment Variable。模板只允许保存密钥文件路径，不能保存私钥内容。

## 生产注意事项

- 本服务的通知表采用 transactional outbox。需要由平台集成任务把 `pending` 记录投递到基础平台通知 API，并在成功后标为 `delivered`。
- API 与 Worker 必须使用相同的 Temporal namespace/task queue 和兼容的 Workflow 代码。上线修改 Workflow 时保留 replay 测试历史；不要直接改动已经执行过的确定性分支。
- `MYSQL_DSN` 必须包含 `parseTime=true`；数据库账号仅授予 `contract_management` 所需权限。
- 审批命令的权限先由 HTTP 用例校验，Workflow 再校验当前处理人/申请人，形成双重业务约束。
