# 合同工作流 Worker 版本发布规范

核对日期：2026-10-13。已贯通配置 → WorkerOptions → Worker 启动/就绪 → rollout 命令 → Temporal 控制面调用；本次仅修改文档，没有执行发布、回滚或新的动态验收。逐项依据见[核验台账](../../.ai/modules/operation-docs-business-20261013.md)。

由具有对应 Temporal namespace/deployment 控制权限的发布人员执行。合同 Worker 支持 Deployment-based Worker Versioning，但配置默认关闭，不能据本文宣称部署已经启用。启用前须确认服务端支持对应控制面、namespace/task queue 一致、新旧 Worker 与镜像关系可追溯。

发布规范要求 Build ID 与不可变镜像唯一对应，不用 latest、分支名或可覆盖标签；当前程序只验证非空，不校验镜像摘要或唯一性，该约束需要发布流程执行。默认 Build ID 为 contract-worker-v1，不能把该默认值作为每次新发布的版本。

## 必需配置

```text
TEMPORAL_ADDRESS=<controlled-temporal-address>
TEMPORAL_NAMESPACE=<authorized-namespace>
TEMPORAL_TASK_QUEUE=<contract-task-queue>
TEMPORAL_WORKER_VERSIONING_ENABLED=true
TEMPORAL_WORKER_DEPLOYMENT_NAME=contract-management
TEMPORAL_WORKER_BUILD_ID=<git-sha-or-image-digest-derived-id>
TEMPORAL_WORKER_VERSIONING_POLICY=PINNED
TEMPORAL_METRICS_ADDRESS=:9091
```

关闭版本化时也要求非空 Build ID；启用时另要求 deployment name，策略只允许 PINNED 或 AUTO_UPGRADE。TLS/API Key 按受控环境注入，不在命令行、日志或仓库粘贴密钥。rollout 命令单独读取环境变量，不自动继承 Worker 的配置默认值。

审批类长工作流默认使用 `PINNED`，避免运行中的审批在未验证情况下切换代码。工作流内部发生
不兼容分支变化时仍必须使用 `workflow.GetVersion`；Worker Versioning 不能替代历史兼容代码。

## 发布顺序

1. 确认迁移、数据库及 Temporal 配置有效，启动新 Build ID Worker；在 Temporal 控制面确认目标版本的 Poller 与任务队列，不能仅看一条启动日志。
2. 检查 Worker 私有指标端口的 `/readyz` 和 `/metrics`。readyz 只反映 Worker 成功启动后的就绪标记，不等于审批业务完成或目标版本已设为 Current。指标来自 SDK 实际上报；未出现某项指标不等于失败数为零，注册表重启后不保留计数。
3. 使用 `worker-rollout` 执行 `RAMP`，先导入 5% 新流量。
4. 观察一个完整业务周期，再依次提升至 25%、50%、100%。每次变更都重新读取 Temporal
   conflict token，避免两个发布任务覆盖对方的路由更新。
5. 100% 稳定后执行 `PROMOTE`，将新 Build ID 设为 Current；命令成功后重新读取控制面，确认实际 Current/Ramping 和测试审批处理版本，命令返回不是完整业务验收。
6. 保留旧 Worker，直到 Temporal 显示旧版本没有 Pinned Workflow 和未完成 Activity 后再缩容。

示例：

```text
TEMPORAL_WORKER_ROLLOUT_ACTION=RAMP
TEMPORAL_WORKER_RAMP_PERCENTAGE=5
TEMPORAL_WORKER_ROLLOUT_IDENTITY=<deployment-id>
./worker-rollout
```

示例须先具备上文 address、namespace、deployment、build ID，及非空 rollout identity；二进制由受控构建提供。RAMP 默认 5%，允许 0～100，不自动按 25/50/100 升级。每个动作先 Describe 取得 conflict token，再更新路由；不是一次命令持续自动观察或灰度。

## 回滚

- 灰度阶段执行 ABORT_RAMP，清空 Ramping 版本并将比例归零。命令入口仍要求非空 TEMPORAL_WORKER_BUILD_ID，即使底层撤销调用传空目标；保留本次受控目标值，不因撤销而删掉该环境变量。
- 已提升为 Current 后，将上一 Build ID 作为目标执行 `PROMOTE`。
- 禁止删除仍有 Pinned Workflow 的 Worker 版本。
- 失败回滚不能删除 `workflow.GetVersion` 的旧分支；只有所有相关历史执行完成后才能清理。

PROMOTE 设置 AllowNoPollers=false、IgnoreMissingTaskQueues=false；RAMP 设置 IgnoreMissingTaskQueues=false，代码没有同名 AllowNoPollers 参数设置，不能笼统声称两个动作都显式禁用无 Poller。两者均无绕过这些显式保护的环境变量；应先验证目标 Poller，再执行操作。

失败处理：参数/TLS 解析失败先修配置；无 Poller/队列缺失先恢复目标 Worker；conflict token 冲突先重新读实际路由，禁止多个发布人盲目反复覆盖。命令控制面超时为 20 秒，超时或网络错误可能无法确定更新是否已完成，先查 Current/Ramping，再决定重试。回滚路由不迁移已经 Pinned 的执行，也不撤销已提交的数据库业务副作用。

## 源码链与成功判定

[配置校验](../internal/config/config.go) → [WorkerOptions](../internal/temporalworker/versioning.go) → [Worker 装配](../cmd/worker/main.go)、[metrics/readyz](../internal/temporalworker/metrics.go)；[rollout 入口](../cmd/worker-rollout/main.go) → [Describe/SetCurrent/SetRamping](../internal/temporalworker/rollout.go)。[审批工作流](../internal/workflows/contract_approval.go) → [Activity](../internal/workflows/activities.go) → [审批事务](../internal/infrastructure/mysql/repository.go)是业务结果链，路由变更本身不直接改合同。

成功需控制面实际路由符合预期、新 Worker 正常处理授权审批且动作/合同状态落库一致，旧 Pinned 执行仍有兼容 Worker 承接。源码、单元测试或 readyz 单项不能替代这些运行确认。
