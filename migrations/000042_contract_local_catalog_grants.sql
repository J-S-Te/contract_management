-- 独立目录可见性，不回填旧合同、目录Owner或任何冻结摘要。
CREATE TABLE IF NOT EXISTS con_local_catalog_grant (
 tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 entry_kind VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 entry_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 subject_type VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 subject_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 subject_name VARCHAR(255) NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT TRUE,
 version BIGINT UNSIGNED NOT NULL,
 updated_by VARCHAR(128) NOT NULL,
 updated_at DATETIME(3) NOT NULL,
 PRIMARY KEY(tenant_id,entry_kind,entry_id,subject_type,subject_id),
 KEY idx_local_catalog_grant_target(tenant_id,subject_type,subject_id,enabled),
 CONSTRAINT chk_local_catalog_grant_kind CHECK (entry_kind IN ('counterparties','categories')),
 CONSTRAINT chk_local_catalog_grant_subject CHECK (subject_type IN ('USER','ORG')),
 CONSTRAINT chk_local_catalog_grant_version CHECK (version>=1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
