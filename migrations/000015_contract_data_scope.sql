ALTER TABLE con_contract
  ADD COLUMN owner_identity_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER customer_phone,
  ADD COLUMN owner_org_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER owner_identity_id,
  ADD COLUMN project_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER owner_org_id,
  ADD KEY idx_con_contract_scope_org (tenant_id, owner_org_id, updated_at),
  ADD KEY idx_con_contract_scope_project (tenant_id, project_id, updated_at),
  ADD KEY idx_con_contract_scope_identity (tenant_id, owner_identity_id, updated_at);
