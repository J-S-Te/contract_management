CREATE TABLE IF NOT EXISTS con_contract_stamped_document (
    contract_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    original_filename VARCHAR(255) NOT NULL,
    content_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    document LONGBLOB NOT NULL,
    uploaded_at DATETIME(3) NOT NULL,
    uploaded_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (contract_id),
    KEY idx_con_stamped_tenant (tenant_id, uploaded_at),
    CONSTRAINT fk_con_stamped_contract FOREIGN KEY (contract_id) REFERENCES con_contract (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
