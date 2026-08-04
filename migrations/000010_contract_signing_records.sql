CREATE TABLE IF NOT EXISTS con_contract_signing (
    contract_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    method VARCHAR(32) NOT NULL DEFAULT 'paper',
    status VARCHAR(32) NOT NULL DEFAULT 'pending_shipment',
    courier_number VARCHAR(120) NULL,
    recipient_name VARCHAR(120) NULL,
    recipient_address VARCHAR(500) NULL,
    mailed_at DATETIME(3) NULL,
    customer_received_at DATETIME(3) NULL,
    seal_verified BOOLEAN NOT NULL DEFAULT FALSE,
    signature_verified BOOLEAN NOT NULL DEFAULT FALSE,
    signed_at DATETIME(3) NULL,
    confirmed_at DATETIME(3) NULL,
    reminder_count INT UNSIGNED NOT NULL DEFAULT 0,
    last_reminded_at DATETIME(3) NULL,
    version BIGINT UNSIGNED NOT NULL DEFAULT 1,
    updated_at DATETIME(3) NOT NULL,
    updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (contract_id),
    KEY idx_con_signing_tenant_status (tenant_id, status, updated_at),
    CONSTRAINT fk_con_signing_contract FOREIGN KEY (contract_id) REFERENCES con_contract (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Existing returned contracts were uploaded before return tracking was introduced. Preserve
-- those files and place them at the manual verification step instead of incorrectly asking
-- users to register shipment again.
INSERT INTO con_contract_signing (
    contract_id, tenant_id, method, status, version, updated_at, updated_by
)
SELECT contract_id, tenant_id, 'paper', 'pending_review', 1, uploaded_at, uploaded_by
FROM con_contract_stamped_document
ON DUPLICATE KEY UPDATE contract_id = VALUES(contract_id);
