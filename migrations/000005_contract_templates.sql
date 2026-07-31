CREATE TABLE con_contract_template (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(160) NOT NULL,
    original_filename VARCHAR(255) NOT NULL,
    fields_json JSON NOT NULL,
    document LONGBLOB NOT NULL,
    created_at DATETIME(3) NOT NULL,
    created_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (id),
    KEY idx_con_template_tenant_created (tenant_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE con_contract
    ADD COLUMN template_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER content,
    ADD COLUMN template_values_json JSON NULL AFTER template_id,
    ADD COLUMN rendered_document LONGBLOB NULL AFTER template_values_json,
    ADD KEY idx_con_contract_template (tenant_id, template_id);
