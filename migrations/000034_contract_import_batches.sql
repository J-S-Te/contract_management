CREATE TABLE IF NOT EXISTS con_import_batch (
 id CHAR(26) NOT NULL,
 tenant_id VARCHAR(64) NOT NULL,
 actor_id VARCHAR(64) NOT NULL,
 state VARCHAR(24) NOT NULL,
 version BIGINT UNSIGNED NOT NULL,
 payload JSON NOT NULL,
 commit_key_hash CHAR(64) NOT NULL DEFAULT '',
 session_hash VARBINARY(32) NULL,
 lease_until DATETIME(6) NULL,
 updated_at DATETIME(6) NOT NULL,
 PRIMARY KEY(id),
 KEY idx_import_dispatch (state,lease_until,updated_at),
 KEY idx_import_actor (tenant_id,actor_id,id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS con_import_identity (
 tenant_id VARCHAR(64) NOT NULL,
 source_system VARCHAR(64) NOT NULL,
 external_record_id VARCHAR(128) NOT NULL,
 digest CHAR(64) NOT NULL,
 contract_id CHAR(26) NULL,
 item_id CHAR(26) NOT NULL,
 created_at DATETIME(6) NOT NULL,
 PRIMARY KEY(tenant_id,source_system,external_record_id),
 UNIQUE KEY uk_import_contract (tenant_id,contract_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
