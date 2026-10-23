-- 盖章文件版本链：同一合同可保留多个不可变文件版本，正式核验必须绑定具体版本与 SHA-256。
-- 旧表每合同一行会在重传时覆盖原文件与哈希；这里改为 (contract_id, version) 主键，
-- 旧版本只标记 superseded，不删除，供历史审计与签署确认追溯。

ALTER TABLE con_contract_stamped_document
    ADD COLUMN version BIGINT UNSIGNED NOT NULL DEFAULT 1 AFTER contract_id,
    ADD COLUMN superseded_at DATETIME(3) NULL AFTER file_gateway_last_error,
    ADD COLUMN superseded_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER superseded_at,
    ADD KEY idx_con_stamped_contract_id (contract_id);

ALTER TABLE con_contract_stamped_document
    DROP PRIMARY KEY,
    ADD PRIMARY KEY (contract_id, version),
    ADD KEY idx_con_stamped_current (tenant_id, contract_id, version);

-- 签署确认绑定到被核验的文件版本与哈希；历史确认单独留存，重传后旧确认仍可追溯。
ALTER TABLE con_contract_signing
    ADD COLUMN confirmed_document_version BIGINT UNSIGNED NULL AFTER confirmed_at,
    ADD COLUMN confirmed_content_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER confirmed_document_version;

CREATE TABLE IF NOT EXISTS con_contract_signing_confirmation (
    id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    contract_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    document_version BIGINT UNSIGNED NOT NULL,
    content_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    seal_verified BOOLEAN NOT NULL DEFAULT FALSE,
    signature_verified BOOLEAN NOT NULL DEFAULT FALSE,
    signed_at DATETIME(3) NOT NULL,
    confirmed_at DATETIME(3) NOT NULL,
    confirmed_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_con_signing_confirmation_version (tenant_id, contract_id, document_version),
    KEY idx_con_signing_confirmation_contract (tenant_id, contract_id, confirmed_at),
    CONSTRAINT fk_con_signing_confirmation_contract FOREIGN KEY (contract_id) REFERENCES con_contract (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 存量已完成签署没有版本列：只有仍能对应唯一文件版本时才回填，避免伪造历史确认。
UPDATE con_contract_signing AS signing
JOIN con_contract_stamped_document AS document
  ON document.contract_id = signing.contract_id AND document.tenant_id = signing.tenant_id
SET signing.confirmed_document_version = document.version,
    signing.confirmed_content_sha256 = document.content_sha256
WHERE signing.status = 'completed'
  AND signing.confirmed_document_version IS NULL;

INSERT INTO con_contract_signing_confirmation (
    id, tenant_id, contract_id, document_version, content_sha256,
    seal_verified, signature_verified, signed_at, confirmed_at, confirmed_by, created_at
)
SELECT CONCAT('01', SUBSTRING(REPLACE(UUID(), '-', ''), 1, 24)),
       signing.tenant_id, signing.contract_id, signing.confirmed_document_version,
       signing.confirmed_content_sha256, signing.seal_verified, signing.signature_verified,
       COALESCE(signing.signed_at, signing.confirmed_at), signing.confirmed_at, signing.updated_by,
       COALESCE(signing.confirmed_at, signing.updated_at)
FROM con_contract_signing AS signing
WHERE signing.status = 'completed'
  AND signing.confirmed_document_version IS NOT NULL
  AND signing.confirmed_at IS NOT NULL
ON DUPLICATE KEY UPDATE contract_id = VALUES(contract_id);
