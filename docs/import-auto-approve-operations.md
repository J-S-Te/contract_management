# 历史导入自动审批：业务知会、开关语义与审计复核

本文对应 2026-11-07 的历史导入自动审批 P0 修复（提交即冻结 AUTO 策略、执行期只认批次快照）。
适用对象：合同业务负责人、合同超级管理员、平台运维、审计/合规。

## 2026-11-09：新历史档案不参与结算开票

新历史存档和历史结清项固定使用 `finance_mode=NONE`。财务目标为
`NOT_APPLICABLE`，不生成结算迁移命令或应收、回款、核销和发票，不等待财务放行。
后续人工结算仍须使用有效财务依据、应收及既有权限；来源“结清”不是财务对账结论。
旧批次保留原策略及已发生事实，不因新规则自动改写；本文以下财务接续描述仅适用于旧批次。

Excel「合同明细」新增“当前负责人”姓名列。预检通过机器目录精确唯一匹配平台用户账号，
并复核可委派范围和交接冻结；找不到、重名、不可委派均逐行阻断。销售姓名和当前负责人
分别解析，不混用销售身份与用户账号。旧Excel缺列时仍可在页面明确选择负责人。
新档案默认通过合同接口同步报表，标准页面和模板不要求选择内部来源通道。

## 1. 必须由业务方书面确认的语义变化

| 维度 | 人工会签（MANUAL，既有行为） | 自动审批（AUTO，本次修复后） |
| --- | --- | --- |
| 确认时机 | 整批全部开立完成后进入会签，全部角色批准后**整批**确认 | 每条历史项开立成功后**逐项**自动确认（按清单声明状态归档） |
| 合同状态 | 会签通过后 `version=1 → 2`、分配系统号 | 同左，但发生在单条开立即刻 |
| 目标投递 | 整批确认后统一生成 PROJECT/FINANCE/REPORT 目标并投递 | 单条确认后即可生成并投递该条的目标（可能早于本批其他项开立完成） |
| 人工审批人 | 必须有受派人且逐角色批准 | 无受派人；系统按租户策略批准，审计主体为 `SYSTEM` |
| 单条合同可投递前置条件 | `version=2` + `import_confirmed_at` 非空 | **不变**（同样两个条件） |

**结论**：单条合同的可投递前置条件、编号规则、归档状态与审计形状不变；变化的只有"目标投递的时序"（从整批后变为逐项）。

业务方需确认（签字/邮件留痕）：

- [ ] 接受 AUTO 批次的目标投递时序为"逐项"，而非"整批确认后统一投递"。
- [ ] 接受 AUTO 批次无人工审批人、审计主体为 `SYSTEM`。
- [ ] 明确哪些租户/期间允许开启自动审批（建议先小范围试点）。
- [ ] 明确紧急停止流程（见 §3，运维刹车是 `pause`，不是关开关）。

## 2. 租户开关的权威性（pin 语义）

- 提交冻结事务内读取租户开关，把判定与 `setting_version` 固化进该批次的确认台账
  （`confirmation.policy_decision` / `policy_version`）。
- 执行期**只认批次快照**（pin）：提交后关闭 env 能力或租户开关，**不会**拦住在途批次，
  否则合同会永久停在 `version=1` 的导入草稿。
- 因此：**开关只影响新提交的批次**；已提交批次的行为在提交那一刻确定。
- 运维若需立即止血，使用 `pause`（见 §3），不要依赖关闭开关。

## 3. 运维刹车与恢复

### 3.1 暂停/恢复

- 接口：`POST /api/v1/contract-import-batches/{id}/control`（`action=pause|resume`，带 `expected_version`）。
- 权限：具备 `contract.import.commit` 的批次创建人或管理员。
- 允许 pause/resume 的确认状态：`STARTING`（人工会签未推进）、`APPROVED` 且 `policy_decision=AUTO`；
  人工 `APPROVED` / `PENDING` / `REJECTED` / `EXPIRED` 一律拒绝，避免把已收敛批次拉回执行态。
- pause 后：调度器不再认领该批次；已提交未完成的目标投递随批次状态停止。
- resume 后：批次回到 `QUEUED`，调度器重新认领；已确认项幂等跳过，不重复取号/写审计。

### 3.2 恢复旧批次（P0 修复前提交、卡在 STARTING/BLOCKED/PARTIAL）

- 应用侧只对"零人工痕迹"的旧确认做锁内升级：`policy_decision` 缺省 + 无命令 + 无已批角色 +
  `STARTING`（未过期）或 `EXPIRED`。有任一人工命令的批次**不会**被自动升级，需人工会签或受控数据修复。
- 操作：先 `resume`；若租户开关仍为自动，升级后逐项归档；若开关已关，则走人工会签路径。
- 只读盘点（排障用，不写库）：

```sql
SELECT id, state,
       JSON_UNQUOTE(JSON_EXTRACT(payload,'$.confirmation.state')) AS cf_state,
       JSON_UNQUOTE(JSON_EXTRACT(payload,'$.confirmation.policy_decision')) AS policy
  FROM con_import_batch
 WHERE JSON_UNQUOTE(JSON_EXTRACT(payload,'$.confirmation.state')) IN ('STARTING','PENDING','EXPIRED')
   AND JSON_SEARCH(payload,'one','IMPORT_REVISION_CONFLICT',NULL,'$.items[*].issues[*].code') IS NOT NULL;
```

## 4. 审计与 digest 复核

- 批次返回的 `confirmation.manifest_digest` 是提交事务内固化的确认范围摘要（按 `{Revision,Items,Sources}`
  计算，不含策略字段）。用于回答"这次自动审批覆盖了哪些清单内容"。
- `CONTRACT_MANAGEMENT:IMPORT_AUTO_APPROVED`：`actor_id=SYSTEM`，payload 含
  `batch_id / item_id / historical_status / source_revision / policy=auto / policy_source=tenant_setting / setting_version`。
- `CONTRACT_MANAGEMENT:IMPORT_CONFIRMED`：自动路径 `actor_id=SYSTEM` 且 payload 含
  `policy=auto / policy_source / policy_version`；旧数据升级另含 `policy_upgraded=true`。人工路径形状不变。
- `CONTRACT_MANAGEMENT:IMPORT_AUTO_APPROVE_SETTING_CHANGED`：记录谁在何时把开关改成什么（含版本号）。
- 复核方法：用批次的 `manifest_digest` + 上述两条事件 + `setting_version` 对齐，确认"哪一版开关、哪批内容、
  由系统确认了哪些项"。

## 5. 未包含在本次修复中的事项（计划，未合入）

- AUTO 批次提交后的 `WITHDRAW` 逃生口（当前撤回只允许人工会签 `STARTING`；AUTO 批次需走 pause + 重新导入）。
- 自动路径审计 `event_id` 的确定性（当前为随机 ULID；重放在业务幂等保护下不会重复写）。
- 前端对 `policy_decision` 的显式展示（当前 `state=APPROVED` 容易被读成人工批准）。
- digest 的对外复核端点（当前从批次返回字段读取，无独立校验接口）。
- 自动审批能力的"装配能力判定"替代部署 env（当前仍由 `CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL` 门禁）。

## 6. 验收清单（部署后）

- [ ] `GET` 批次可见 `confirmation.state / policy_decision / policy_version / manifest_digest`。
- [ ] AUTO 批次提交后：无 Temporal 工作流仍返回受理；`DispatchImportOnce` 后合同 `version=2`、
      状态=清单声明状态、系统号非空、`import_confirmed_at` 非空。
- [ ] 批次最终进入 `TARGETS_PENDING`（不是 `CONFIRMATION_PENDING`）。
- [ ] 提交后关闭租户开关，在途批次仍完成（pin 生效）。
- [ ] MANUAL 批次行为与修复前一致；人工会签路径回归通过。
- [ ] 审计两条事件 actor/policy 字段符合 §4。
