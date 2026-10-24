-- 人员离职交接机器协议：责任冻结与幂等回执。
-- 业务责任仍由合同系统持有；本组表只记录"该用户在交接期间不得再被分配新责任"与逐项转移回执。

CREATE TABLE IF NOT EXISTS con_handover_freeze (
  id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  batch_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  request_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'FROZEN',
  frozen_at DATETIME(3) NOT NULL,
  released_at DATETIME(3) NULL,
  outcome VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_con_handover_freeze_batch_user (tenant_id, batch_id, user_id),
  KEY idx_con_handover_freeze_active (tenant_id, user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS con_handover_receipt (
  id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  batch_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  item_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  resource_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  resource_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  from_user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  to_user_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  idempotency_key VARCHAR(160) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  applied_version BIGINT UNSIGNED NOT NULL DEFAULT 0,
  reason_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
  evidence_ref VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL,
  received_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_con_handover_receipt_key (tenant_id, idempotency_key),
  KEY idx_con_handover_receipt_item (tenant_id, item_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
