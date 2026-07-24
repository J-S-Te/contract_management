# CON 合同状态机与审批工作流

本仓库实现 `原始需求.md` 中 CON-002、APP-001、APP-002、APP-004 和 APP-005 的后端核心能力。技术栈为 Go、GORM、MySQL 和 Temporal；HTTP 接口沿用基础平台的 `/api/v1`、统一响应包裹和 HttpOnly Cookie 会话。

## 已实现能力

- 9 状态合同状态机：`draft → pending → approved → active → in_progress → pending_pay → completed → archived`，并支持履约阶段终止后归档。
- 普通状态直接流转；执行中、待付款、终止、归档走管理员状态变更审批。
- 合同提交后的多级审批。默认节点为销售总监、技术总监、财务总监；审批规则可按金额、服务类型、客户信用、合同类型和条款一致性组合 AND/OR 条件，并按优先级选择唯一规则。
- Temporal 长流程：72 小时节点超时、定时提醒、通过、拒绝、会签/或签、转交、退回历史节点、申请人撤回、评论和催办。
- 提交时固化规则版本、节点和合同 SHA-256；运行中规则变更不影响既有流程，审批完成前合同内容发生变化会阻断生效。
- MySQL 事务保存合同状态、生命周期事件、审批实例、任务和动作；Activity、通知 outbox 均有幂等键，可安全重试。
- Worker 启动时确保每日自动归档 Cron Workflow 存在；默认北京时间零点执行，通知合同负责人、销售总监角色和管理员角色。
- 基础平台认证适配器通过 `/api/v1/auth/me` 校验配置的浏览器会话 Cookie（默认 `bp_session`），不接受客户端伪造的用户或租户 ID。
- 配置 `PLATFORM_AUDIT_CLIENT_ID`、`PLATFORM_AUDIT_CLIENT_SECRET`、`PLATFORM_APPLICATION_CODE` 和 `PLATFORM_ENVIRONMENT_CODE` 后，合同写操作会以 OAuth Client Credentials 和 `audit.ingest` scope 写入基础平台审计。

## 目录

```text
cmd/api                         HTTP 服务
cmd/worker                      Temporal Worker
internal/domain/contract        状态机和合同模型
internal/domain/approval        审批模型与规则表达式引擎
internal/workflows              Temporal Workflows / Activities
internal/application            用例、权限和工作流启动/Signal
internal/infrastructure/mysql   GORM 模型、事务存储与 outbox
internal/infrastructure/platform 基础平台会话校验
internal/transport/httpapi      REST API
migrations                      MySQL DDL
```

Temporal Workflow 只做确定性编排，时间使用 `workflow.Now/NewTimer`；数据库、通知和审计事实都由可重试 Activity 完成。持久化统一使用 GORM，悲观锁使用 `clause.Locking`，幂等写入使用 `clause.OnConflict`；生产表结构仍以显式 SQL 迁移为准，不在服务启动时执行 `AutoMigrate`。

## 状态流转

```text
draft -> pending -> approved -> active -> in_progress -> pending_pay -> completed -> archived
                                      \             \               \
                                       +-------------+----> terminated -> archived
```

系统内部自动步骤为 `pending → approved → active`。`in_progress`、`pending_pay`、`terminated`、`archived` 必须启动 `contract-status-change` Workflow；`completed` 等非关键目标可在状态机校验和乐观锁保护下直接变更。

## HTTP API

所有业务接口都需要平台 Cookie；成功/失败响应遵循 `{code,message,request_id,data}`。

| 方法 | 路径 | 权限 | 行为 |
|---|---|---|---|
| POST | `/api/v1/contracts` | `contract.create` | 创建草稿并计算正文 SHA-256 |
| GET | `/api/v1/contracts/{id}` | `contract.read` | 查询合同；普通用户只能读取自己的合同 |
| POST | `/api/v1/contracts/{id}/submit-approval` | `contract.create` | 匹配规则并启动合同审批 |
| POST | `/api/v1/contracts/{id}/status-changes` | `contract.edit` | 直接流转或启动关键状态审批 |
| GET | `/api/v1/approvals/tasks` | `approval.process` | 当前用户待办 |
| GET | `/api/v1/approvals/{id}` | `approval.view` 或申请人 | 查询 Temporal 当前流程状态 |
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

## 本地运行

要求 Go 1.25.4+、MySQL 8.4+ 和 Temporal Server。复制配置后启动基础依赖：

```bash
cp .env.example .env
docker compose up -d mysql temporal temporal-ui
set -a; source .env; set +a
go run ./cmd/worker
go run ./cmd/api
```

MySQL 初始化会执行 `migrations/000001_contract_workflow.sql`。Temporal UI 位于 `http://localhost:8233`。API 默认 `:8081`，基础平台默认 `http://localhost:8080`；部署时由网关把合同路径转发至本服务。

审批人按角色在 `APPROVER_ROLE_ASSIGNMENTS_JSON` 中配置，值必须使用平台用户 ULID。生产环境建议由配置中心下发；不要在镜像或仓库中保存真实人员 ID、Temporal API Key 或数据库密码。

平台会话 Cookie 名称由 `AUTH_SESSION_COOKIE_NAME` 配置，默认 `bp_session`。审计凭据必须由密钥管理系统注入；未完整配置审计的四项环境变量时，审计投递保持禁用。通知 outbox 不会直接调用平台通知控制面，因为当前机器 Token 的已发布权限边界仅包含审计写入。

Temporal Cloud 可设置 `TEMPORAL_TLS=true`、`TEMPORAL_API_KEY`、命名空间和地址。API Key 会通过 SDK Credentials 注入，不写入 Workflow history。

## 验证

```bash
make fmt
make test
make vet
make build
```

领域测试覆盖合法/非法状态流转与规则优先级；Temporal 测试覆盖三级通过、撤回和关键状态审批通过。数据库集成测试应在 CI 中对迁移后的 MySQL 执行。

## 生产注意事项

- 本服务的通知表采用 transactional outbox。需要由平台集成任务把 `pending` 记录投递到基础平台通知 API，并在成功后标为 `delivered`。
- API 与 Worker 必须使用相同的 Temporal namespace/task queue 和兼容的 Workflow 代码。上线修改 Workflow 时保留 replay 测试历史；不要直接改动已经执行过的确定性分支。
- `MYSQL_DSN` 必须包含 `parseTime=true`；数据库账号仅授予 `contract_management` 所需权限。
- 审批命令的权限先由 HTTP 用例校验，Workflow 再校验当前处理人/申请人，形成双重业务约束。
