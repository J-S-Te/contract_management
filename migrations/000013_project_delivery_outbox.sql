CREATE TABLE IF NOT EXISTS con_project_delivery_outbox (
  id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  contract_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  contract_version BIGINT UNSIGNED NOT NULL,
  payload_json JSON NOT NULL,
  delivery_status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'pending',
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(3) NOT NULL,
  locked_at DATETIME(3) NULL,
  delivered_at DATETIME(3) NULL,
  last_error VARCHAR(1000) NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_con_project_delivery_version (tenant_id, contract_id, contract_version),
  KEY idx_con_project_delivery_due (delivery_status, next_attempt_at),
  CONSTRAINT fk_con_project_delivery_contract FOREIGN KEY (contract_id) REFERENCES con_contract(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
