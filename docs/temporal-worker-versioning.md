# 合同工作流 Worker 版本发布规范

合同 Worker 使用 Temporal Deployment-based Worker Versioning。`BuildID` 必须是不可变镜像
对应的唯一版本，不允许使用 `latest`、分支名或会被覆盖的标签。

## 必需配置

```text
TEMPORAL_WORKER_VERSIONING_ENABLED=true
TEMPORAL_WORKER_DEPLOYMENT_NAME=contract-management
TEMPORAL_WORKER_BUILD_ID=<git-sha-or-image-digest-derived-id>
TEMPORAL_WORKER_VERSIONING_POLICY=PINNED
TEMPORAL_METRICS_ADDRESS=:9091
```

审批类长工作流默认使用 `PINNED`，避免运行中的审批在未验证情况下切换代码。工作流内部发生
不兼容分支变化时仍必须使用 `workflow.GetVersion`；Worker Versioning 不能替代历史兼容代码。

## 发布顺序

1. 启动新 Build ID Worker，并确认该版本已经注册 Poller。
2. 检查 `/metrics` 中 workflow task failure、activity failure、workflow failure 与 Poller 指标。
3. 使用 `worker-rollout` 执行 `RAMP`，先导入 5% 新流量。
4. 观察一个完整业务周期，再依次提升至 25%、50%、100%。每次变更都重新读取 Temporal
   conflict token，避免两个发布任务覆盖对方的路由更新。
5. 100% 稳定后执行 `PROMOTE`，将新 Build ID 设为 Current。
6. 保留旧 Worker，直到 Temporal 显示旧版本没有 Pinned Workflow 和未完成 Activity 后再缩容。

示例：

```text
TEMPORAL_WORKER_ROLLOUT_ACTION=RAMP
TEMPORAL_WORKER_RAMP_PERCENTAGE=5
TEMPORAL_WORKER_ROLLOUT_IDENTITY=<deployment-id>
./worker-rollout
```

## 回滚

- 灰度阶段执行 `ABORT_RAMP`，将新版本流量归零。
- 已提升为 Current 后，将上一 Build ID 作为目标执行 `PROMOTE`。
- 禁止删除仍有 Pinned Workflow 的 Worker 版本。
- 失败回滚不能删除 `workflow.GetVersion` 的旧分支；只有所有相关历史执行完成后才能清理。

`worker-rollout` 默认禁止无 Poller版本和缺少任务队列的版本被提升，不提供跳过该保护的环境变量。
