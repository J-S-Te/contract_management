ALTER TABLE con_oidc_login_transaction
    ADD COLUMN central_ticket_ciphertext VARBINARY(1024) NULL,
    ADD COLUMN force_login BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE con_oidc_session
    ADD COLUMN central_binding_ciphertext VARBINARY(1024) NULL,
    ADD COLUMN central_session_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN oidc_sid VARCHAR(256) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD KEY idx_con_oidc_sid (tenant_id,oidc_sid);
