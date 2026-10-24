-- 审批提交快照：审批历史必须展示发起时的合同正文/文档版本、金额币种、付款计划、
-- 客户引用与规则版本，而不是合同当前草稿。快照只在 StartApproval 事务内写入一次。

CREATE TABLE IF NOT EXISTS con_approval_snapshot (
    approval_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    contract_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    contract_version BIGINT UNSIGNED NOT NULL,
    content_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    contract_json JSON NOT NULL,
    rendered_document LONGBLOB NULL,
    rule_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    rule_version BIGINT UNSIGNED NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (approval_id),
    KEY idx_con_approval_snapshot_contract (tenant_id, contract_id, created_at),
    CONSTRAINT fk_con_approval_snapshot_instance FOREIGN KEY (approval_id) REFERENCES con_approval_instance (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
