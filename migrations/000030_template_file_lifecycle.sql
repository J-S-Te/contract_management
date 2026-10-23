-- Persist immutable source generations before any gateway side effect.
-- No foreign key: retired sources must survive physical template deletion.
ALTER TABLE con_contract_template
    ADD COLUMN gateway_source_id VARCHAR(26) NOT NULL DEFAULT '' AFTER file_gateway_last_error;

CREATE TABLE con_template_file_outbox (
    id VARCHAR(26) NOT NULL PRIMARY KEY,
    tenant_id VARCHAR(26) NOT NULL,
    template_id VARCHAR(26) NOT NULL,
    file_id VARCHAR(64) NOT NULL DEFAULT '',
    filename VARCHAR(255) NOT NULL,
    resource_type VARCHAR(64) NOT NULL,
    resource_id VARCHAR(64) NOT NULL,
    document LONGBLOB NOT NULL,
    retired BOOLEAN NOT NULL DEFAULT FALSE,
    delivery_status VARCHAR(16) NOT NULL DEFAULT 'pending',
    attempts BIGINT UNSIGNED NOT NULL DEFAULT 0,
    next_attempt_at DATETIME(6) NOT NULL,
    last_error VARCHAR(64) NOT NULL DEFAULT '',
    created_at DATETIME(6) NOT NULL,
    INDEX idx_template_file_due (delivery_status, next_attempt_at),
    INDEX idx_template_file_source (tenant_id, template_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Existing rows retain their original resource identity. This is a source
-- registry/backfill, not an assertion that historical orphan bindings are known.
INSERT INTO con_template_file_outbox
    (id, tenant_id, template_id, file_id, filename, resource_type, resource_id, document,
     retired, delivery_status, next_attempt_at, created_at)
SELECT id, tenant_id, id, platform_file_id, original_filename, 'contract_template', id, document,
       FALSE, 'pending', UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)
FROM con_contract_template WHERE platform_file_id <> '';

UPDATE con_contract_template SET gateway_source_id = id WHERE platform_file_id <> '';
