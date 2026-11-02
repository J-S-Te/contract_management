-- 合同摘要 linked_at 第 3 层证据索引（BIZ-linked_at）。
--
-- 商机关联确认动作在同一业务事务里写 con_audit_outbox（见 opportunity_link_guard.go
-- 的 insertAuditOutbox：ResourceType='contract'、ResourceID=合同 ID、
-- Action='CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED'）。合同摘要接口按
-- (tenant_id, resource_type, resource_id, action, result) 查这条流水补证；
-- 缺少该索引时每次摘要请求都会扫整张审计 Outbox，签约核验高峰期直接变慢查询。
--
-- 守卫式、可重入：本仓 runner 按版本 + checksum 记账，但 DDL 隐式提交后、记账前
-- 中断时迁移会重跑。重跑只校验既有索引定义，不重复加键、不改任何数据。
DROP PROCEDURE IF EXISTS con_ensure_audit_outbox_resource_index;
CREATE PROCEDURE con_ensure_audit_outbox_resource_index()
BEGIN
  DECLARE index_count INT DEFAULT 0;
  DECLARE index_columns TEXT DEFAULT '';
  DECLARE index_non_unique INT DEFAULT 0;
  SELECT COUNT(*), COALESCE(GROUP_CONCAT(column_name ORDER BY seq_in_index), ''), COALESCE(MAX(non_unique), 0)
    INTO index_count, index_columns, index_non_unique
    FROM information_schema.statistics
   WHERE table_schema = DATABASE() AND table_name = 'con_audit_outbox'
     AND index_name = 'idx_con_audit_outbox_resource';
  IF index_count > 0 AND
     (index_count <> 5 OR index_columns <> 'tenant_id,resource_type,resource_id,action,created_at'
      OR index_non_unique <> 1) THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT = 'con_audit_outbox resource index definition mismatch; inspect schema before retry';
  END IF;
  IF index_count = 0 THEN
    ALTER TABLE con_audit_outbox
      ADD KEY idx_con_audit_outbox_resource (tenant_id, resource_type, resource_id, action, created_at);
  END IF;
END;
CALL con_ensure_audit_outbox_resource_index();
DROP PROCEDURE con_ensure_audit_outbox_resource_index;
