ALTER TABLE con_approval_instance
    ADD COLUMN active_status_change_key VARCHAR(53) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD UNIQUE KEY uk_con_approval_active_status_change (active_status_change_key);

UPDATE con_approval_instance
SET active_status_change_key = CONCAT(tenant_id, ':', contract_id)
WHERE kind = 'status_change' AND status = 'running';
