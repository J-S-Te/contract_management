ALTER TABLE con_opportunity_intake
  ADD COLUMN contract_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER contract_ref,
  ADD COLUMN contract_number VARCHAR(128) NULL AFTER contract_id;

CREATE TABLE con_opportunity_link_outbox (
  id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id VARCHAR(128) NOT NULL,
  intake_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  payload_json JSON NOT NULL,
  delivery_status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'pending',
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(3) NOT NULL,
  locked_at DATETIME(3) NULL,
  delivered_at DATETIME(3) NULL,
  last_error VARCHAR(1000) NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_con_opportunity_link_event (tenant_id, event_id),
  KEY idx_con_opportunity_link_due (delivery_status, next_attempt_at),
  CONSTRAINT fk_con_opportunity_link_intake FOREIGN KEY (intake_id) REFERENCES con_opportunity_intake(intake_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
