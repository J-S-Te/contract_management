# `linked_at` 历史盘点（只读 SQL，D2）

> 关联决策：`.ai/qa/business-logic-audit-20261030/fix-plans/L2-data-portal.md` 第 7 节 B 部分、
> `DECISIONS-SIGNOFF.md` D2。本文件只提供**只读**盘点语句，不写业务数据、不执行回填。
> 代码路径不依赖回填结果：即使从未回填，接口也会把证据精度显式标注为 `approx`。

## 1. 口径（与实现一致）

合同摘要 `linked_at` 的证据链在 `contract_management/internal/infrastructure/mysql/contract_summary.go`
（`contractSummaryLinkedAt`）中固定为四层，命中一层即返回：

| 优先级 | `linked_at_source` | 证据 | 精度 |
|---|---|---|---|
| 1 | `CRM_REVIEW` | `con_opportunity_intake.reviewed_at`（`status='LINK_CONFIRMED'` 且 `contract_id` 指向该合同） | `exact` |
| 2 | `REVIEW_LOG` | `con_opportunity_intake_review.created_at`（同接入单、`decision='LINK_CONFIRMED'`） | `exact` |
| 3 | `AUDIT_LOG` | `con_audit_outbox.created_at`（`action='CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED'`、`result='SUCCESS'`） | `exact` |
| 4 | `INTAKE_CREATED` | `con_opportunity_intake.created_at`（`status='LINK_CONFIRMED'`） | `approx`（早于确认动作，只是下限） |
| 兜底 | `APPROX_CONTRACT_UPDATED` | `con_contract.updated_at` | `approx`（可能晚于真实关联时刻） |

因此“**只有 `updated_at` 证据**”等价于：**既没有第 3 层审计流水，也没有任何
`LINK_CONFIRMED` 的接入单**（第 1/2/4 层都依赖那类接入单）。这类合同对外返回的
`linked_at` 是 `updated_at`，`linked_at_precision='approx'`、`linked_at_source='APPROX_CONTRACT_UPDATED'`。

`linked_at` 是“最新一次可证明关联时间”，`first_linked_at` 是同一证据层内“最早一次”；
多张接入单场景两者会不同。任何一层命中时 `linked_at` 都不会使用 `updated_at`。

## 2. 只读与安全约束

- 全部语句只有 `SELECT`，不含 `INSERT/UPDATE/DELETE/ALTER`、不含 `FOR UPDATE`/锁提示、不创建临时表。
- 每条语句都强制 `tenant_id = :tenant_id`；**禁止**去掉租户条件做跨租户统计。
- 建议在只读副本执行，并按 `:limit` 分页；生产高峰期不要对 `con_audit_outbox` 做全表聚合。
- 第 3 层需要 `idx_con_audit_outbox_resource (tenant_id, resource_type, resource_id, action, created_at)`
  （迁移 `000040_con_audit_outbox_resource_index.sql`）。索引缺失时本盘点与线上摘要查询都会退化为大范围扫描，
  应先确认索引存在：

```sql
SELECT INDEX_NAME, SEQ_IN_INDEX, COLUMN_NAME
FROM information_schema.statistics
WHERE table_schema = DATABASE()
  AND table_name = 'con_audit_outbox'
  AND index_name = 'idx_con_audit_outbox_resource'
ORDER BY SEQ_IN_INDEX;
```

## 3. 汇总计数：`linked_at` 仅有 `updated_at` 证据的合同

```sql
-- 参数：:tenant_id
SELECT COUNT(*) AS approx_linked_contracts
FROM con_contract AS c
WHERE c.tenant_id = :tenant_id
  -- 只看 CRM 关联合同：没有 CRM 客户/商机的合同不会被合同摘要按关联语义核验。
  AND (c.crm_customer_id IS NOT NULL OR (c.opportunity_id IS NOT NULL AND c.opportunity_id <> ''))
  AND NOT EXISTS (
        SELECT 1 FROM con_audit_outbox AS a
        WHERE a.tenant_id = c.tenant_id
          AND a.resource_type = 'contract'
          AND a.resource_id = c.id
          AND a.action = 'CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED'
          AND a.result = 'SUCCESS')
  AND NOT EXISTS (
        SELECT 1 FROM con_opportunity_intake AS i
        WHERE i.tenant_id = c.tenant_id
          AND i.contract_id = c.id
          AND i.status = 'LINK_CONFIRMED');
```

## 4. 清单：需要关注的合同（按 `updated_at` 倒序）

```sql
-- 参数：:tenant_id, :limit
SELECT c.tenant_id,
       c.id                AS contract_id,
       c.contract_number,
       c.status,
       c.crm_customer_id,
       c.opportunity_id,
       c.updated_at        AS linked_at_fallback_updated_at,
       'APPROX_CONTRACT_UPDATED' AS linked_at_source,
       'approx'            AS linked_at_precision
FROM con_contract AS c
WHERE c.tenant_id = :tenant_id
  AND (c.crm_customer_id IS NOT NULL OR (c.opportunity_id IS NOT NULL AND c.opportunity_id <> ''))
  AND NOT EXISTS (
        SELECT 1 FROM con_audit_outbox AS a
        WHERE a.tenant_id = c.tenant_id
          AND a.resource_type = 'contract'
          AND a.resource_id = c.id
          AND a.action = 'CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED'
          AND a.result = 'SUCCESS')
  AND NOT EXISTS (
        SELECT 1 FROM con_opportunity_intake AS i
        WHERE i.tenant_id = c.tenant_id
          AND i.contract_id = c.id
          AND i.status = 'LINK_CONFIRMED')
ORDER BY c.updated_at DESC
LIMIT :limit;
```

## 5. 证据分层分布：核对“哪些合同其实是精确的”

用于确认第 1–4 层是否被正确识别（同一批合同上逐层求值，仅用于盘点与回归核对）。

```sql
-- 参数：:tenant_id, :limit
SELECT c.id AS contract_id,
       c.contract_number,
       (SELECT COUNT(*) FROM con_opportunity_intake AS i
         WHERE i.tenant_id = c.tenant_id AND i.contract_id = c.id
           AND i.status = 'LINK_CONFIRMED' AND i.reviewed_at IS NOT NULL)          AS crm_review_rows,
       (SELECT COUNT(*) FROM con_opportunity_intake_review AS r
          JOIN con_opportunity_intake AS i
            ON i.intake_id = r.intake_id AND i.tenant_id = r.tenant_id
         WHERE i.tenant_id = c.tenant_id AND i.contract_id = c.id
           AND i.status = 'LINK_CONFIRMED' AND r.decision = 'LINK_CONFIRMED')      AS review_log_rows,
       (SELECT COUNT(*) FROM con_audit_outbox AS a
         WHERE a.tenant_id = c.tenant_id AND a.resource_type = 'contract'
           AND a.resource_id = c.id
           AND a.action = 'CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED'
           AND a.result = 'SUCCESS')                                              AS audit_log_rows,
       (SELECT COUNT(*) FROM con_opportunity_intake AS i
         WHERE i.tenant_id = c.tenant_id AND i.contract_id = c.id
           AND i.status = 'LINK_CONFIRMED')                                       AS linked_intake_rows
FROM con_contract AS c
WHERE c.tenant_id = :tenant_id
  AND (c.crm_customer_id IS NOT NULL OR (c.opportunity_id IS NOT NULL AND c.opportunity_id <> ''))
ORDER BY c.id
LIMIT :limit;
```

判读方式（与代码优先级一致）：

- `crm_review_rows > 0` → `CRM_REVIEW`（`exact`）；
- 否则 `review_log_rows > 0` → `REVIEW_LOG`（`exact`）；
- 否则 `audit_log_rows > 0` → `AUDIT_LOG`（`exact`）；
- 否则 `linked_intake_rows > 0` → `INTAKE_CREATED`（`approx`，是下限而不是确认时刻）；
- 均为 0 → 走 `updated_at` 兜底（`APPROX_CONTRACT_UPDATED`）。

## 6. 回填候选（只读预览，**不执行**）

若后续批准 D2 的受控回填，第一步只能先看这份预览：哪些 `LINK_CONFIRMED` 接入单缺少
`reviewed_at`（第 1 层缺失）。回填取值优先级与第 1–3 层一致，必须业务批准 + 受控执行 + 审计留痕；
且代码路径不得依赖回填结果。

```sql
-- 参数：:tenant_id, :limit
SELECT i.tenant_id,
       i.intake_id,
       i.contract_id,
       i.contract_number,
       i.reviewed_at,
       i.updated_at,
       (SELECT MIN(r.created_at) FROM con_opportunity_intake_review AS r
         WHERE r.tenant_id = i.tenant_id AND r.intake_id = i.intake_id
           AND r.decision = 'LINK_CONFIRMED')                                     AS earliest_review_log_at,
       (SELECT MIN(a.created_at) FROM con_audit_outbox AS a
         WHERE a.tenant_id = i.tenant_id AND a.resource_type = 'contract'
           AND a.resource_id = i.contract_id
           AND a.action = 'CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED'
           AND a.result = 'SUCCESS')                                              AS earliest_audit_at
FROM con_opportunity_intake AS i
WHERE i.tenant_id = :tenant_id
  AND i.status = 'LINK_CONFIRMED'
  AND i.contract_id IS NOT NULL
  AND i.reviewed_at IS NULL
ORDER BY i.updated_at DESC
LIMIT :limit;
```

## 7. 已知限制

- `updated_at` 会随任意合同编辑（含状态流转、草稿保存）变化，因此 `approx` 值可能明显晚于真实关联时刻；
  不能用于结算/审计的先后判断。
- 接入单被清理或迁移、且审计流水也未落库的合同无法恢复精确时间，只能维持 `approx`。
- 本盘点不修改任何数据；是否回填仍是数据治理决策（D2），不因本轮实现而默认执行。
