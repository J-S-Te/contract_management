# Commercial runtime enforcement

The controlled deployment defaults to `COMMERCIAL_LICENSE_ENABLED=false` for
compatibility during rollout. API and Worker always construct a shared consumer
gate. Explicit `true` requires valid trust, binding, durable state and independent
machine credentials; missing configuration fails startup, never reverts to false.
Do not manually enable before platform registration and snapshot activation.

## Independent API and Worker components

Controlled production deployment uses two services from the same immutable image:

| Component | Command | Runtime credential file | State volume |
| --- | --- | --- | --- |
| `contract-api` | `./api` | `runtime/license-contract-api.env` | API-only volume |
| `contract-worker` | `./worker` | `runtime/license-contract-worker.env` | Worker-only volume |

Both set `CONTRACT_PROCESS_MODE=split` and
`CONTRACT_RUN_WORKER_WITH_API=false`. The API no longer starts business CRM/project
dispatchers in this mode; the Worker owns them and the Temporal Activities. The API
retains existing notification delivery and audit functionality. Each process checks
its compiled component identity against `COMMERCIAL_LICENSE_SERVICE_ID` before any
database or Temporal connection, then runs its own durable consumer and snapshot ACK.
Credentials and state volumes must never be shared between these two services.

The Worker exposes internal `GET /readyz` on `TEMPORAL_METRICS_ADDRESS` (default
`:9091`), returning 503 before Temporal polling starts or during shutdown. License
expiry does not make the Worker unhealthy: business execution pauses in its checks,
while snapshot recovery and already-running workflow timers remain alive. Do not
publish this metrics/readiness port externally.

Legacy deployment without commercial licensing keeps the original default
`CONTRACT_PROCESS_MODE=legacy`; `CONTRACT_RUN_WORKER_WITH_API=true` can still supervise
both processes for old installations. Licensed runtimes require split mode. Split
mode combined with the legacy embedded switch fails startup rather than creating a
duplicate Worker. Replace the old combined container, register both components and
approve both image/coverage bindings before enabling licensing. A single old API ACK
does not stand in for the new Worker ACK.

Runtime configuration is supplied by the platform's controlled onboarding flow:

- `COMMERCIAL_LICENSE_ENABLED`
- `COMMERCIAL_LICENSE_INSTANCE_ID`, `COMMERCIAL_LICENSE_ENVIRONMENT`
- `COMMERCIAL_LICENSE_SERVICE_ID`, `COMMERCIAL_LICENSE_STATE_PATH`
- `COMMERCIAL_LICENSE_PLATFORM_PUBLIC_KEY_PATH`
- `COMMERCIAL_LICENSE_PLATFORM_BASE_URL`, `COMMERCIAL_LICENSE_ALLOW_HTTP`
- `COMMERCIAL_LICENSE_CLIENT_ID`, `COMMERCIAL_LICENSE_CLIENT_SECRET`
- `COMMERCIAL_LICENSE_COVERAGE_DIGEST`, `COMMERCIAL_LICENSE_IMAGE_DIGEST`

Only a platform public key is delivered. Vendor trust is compiled into the audited
shared module; neither runtime credentials nor private keys belong in Git/images.
The consumer polls at 30 seconds and acknowledges only durably applied signed
snapshots. Absolute expiry is evaluated locally even when the platform is offline.

HTTP uses reviewed exact route semantics, not a GET/POST-wide rule. Template POST
preview and contract historical downloads remain read/export operations. Unknown
paths require business entitlement. Existing authentication, scope, same-origin
and audit checks remain in place. Service mutation methods recheck independently
before persistence or workflow dispatch, so bypassing UI/router is not sufficient.

All approval/status/archive/notification-creation Activities recheck at execution.
A specific license restriction pauses the durable workflow using a 30-second
timer instead of consuming its ordinary eight-attempt failure budget. Recovery
retries the Activity; other errors retain the existing retry policy. Archive cron
registration rechecks before scheduling and resumes without stopping the Worker.

CRM link and project activation are business handoffs: license is checked before
claim and immediately before the external call. Denial does not consume a business
retry or mark the delivery terminal; a previously acquired lease may expire and be
retried after recovery. Existing audit and notification delivery continue normally.
Financial-effective delivery and template file lifecycle work are also rechecked by their respective execution gates. File backfill CLI checks each row. Template importer uses the authenticated API,
so template writes are governed by the same HTTP/service gate.

Standalone repository builds include only reviewed root/runtime/syncclient/consumer
sources in `third_party/license-core`; `scripts/license-core-sync.sh --check`
validates the exact file allowlist and digest manifest. Docker performs that check
before module download/build. Runtime enabling, full installation evidence and
browser/deployment acceptance are separate release gates, not implied by tests.

## 操作步骤与验收（2026-10-22 源码核验）

部署管理员通过固定批准和受控接入登记 contract-api/contract-worker，平台许可管理员导入同实例商业许可后在“运行时授权执行”选“合同管理”、刷新并确认激活。不要手工把 API 凭据给 Worker。成功按部署 READY、ENFORCED、snapshot_current=true及两个成员当期修订 ACK判断，进程 readyz 只证明准备状态。

到期立即暂停新合同、审批推进、上传、CRM/项目交接及自动归档，不追加写入宽限；原授权项仍存在且已生效、可信快照和原业务权限正常时才可查询及导出历史。合同页面已有 DOCX/PDF/签章下载走前端 contract_management/api → router.go 认证/同源/操作分类 → application Service 及仓储/文件网关，不能用“GET一律只读”推断。审批命令经 Service 再检查、Temporal Activities 在 Store持久化前复核；许可暂停走30秒持久timer，不假成功、不消耗业务重试。

失败查真实合同权限/角色、当前许可证、两个进程独立机器配置与状态卷、Temporal和外部集成，不停整个Worker来实现只读。许可恢复后由原工作流/队列重试，外部回执仍需核对。

依据：cmd/api及cmd/worker main → internal/bootstrap/license.go → internal/transport/httpapi/router.go和license.go → internal/application/service.go、license.go → internal/workflows/activities.go、工作流timer及repository持久化；生产contract_management-prod.yaml/Compose分离装配。本次未部署、执行审批或动态验收。
