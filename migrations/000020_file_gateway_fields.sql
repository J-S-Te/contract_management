-- 保留原始 BLOB 作为 N-1 回退，逐步记录平台文件网关绑定状态。
ALTER TABLE con_contract_stamped_document
    ADD COLUMN platform_file_id VARCHAR(64) NOT NULL DEFAULT '' AFTER uploaded_by,
    ADD COLUMN file_gateway_state VARCHAR(16) NOT NULL DEFAULT 'DISABLED' AFTER platform_file_id,
    ADD COLUMN file_gateway_last_error VARCHAR(512) NOT NULL DEFAULT '' AFTER file_gateway_state,
    ADD KEY idx_con_stamped_file_gateway (tenant_id, file_gateway_state, uploaded_at);
