ALTER TABLE con_contract
    ADD COLUMN source_file_id VARCHAR(64) NOT NULL DEFAULT '' AFTER rendered_document,
    ADD COLUMN source_file_status VARCHAR(16) NOT NULL DEFAULT '' AFTER source_file_id,
    ADD COLUMN source_file_last_error VARCHAR(512) NOT NULL DEFAULT '' AFTER source_file_status,
    ADD INDEX idx_con_contract_source_gateway (tenant_id, source_file_status, id);

ALTER TABLE con_contract_template
    ADD COLUMN platform_file_id VARCHAR(64) NOT NULL DEFAULT '' AFTER document,
    ADD COLUMN file_gateway_state VARCHAR(16) NOT NULL DEFAULT '' AFTER platform_file_id,
    ADD COLUMN file_gateway_last_error VARCHAR(512) NOT NULL DEFAULT '' AFTER file_gateway_state,
    ADD INDEX idx_con_template_gateway (tenant_id, file_gateway_state, id);
