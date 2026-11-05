-- 新列保留NULL：不回填旧身份、不重写旧合同或冻结导入摘要。
DROP PROCEDURE IF EXISTS con_expand_local_catalog;
-- con_expand_local_catalog 可恢复地扩展本地引用列，不回填或更改旧身份。
-- 参数：无，作用域为当前合同数据库。
-- 返回值：无；列组或索引不完整时失败关闭。
CREATE PROCEDURE con_expand_local_catalog()
BEGIN
 DECLARE column_count INT DEFAULT 0;
 DECLARE index_columns TEXT DEFAULT '';
 SELECT COUNT(*) INTO column_count FROM information_schema.columns
 WHERE table_schema=DATABASE() AND table_name='con_contract'
 AND column_name IN ('counterparty_source','local_counterparty_id','counterparty_version','category_source','integration_policy');
 IF column_count=0 THEN
  ALTER TABLE con_contract
   ADD COLUMN counterparty_source VARCHAR(16) NULL,
   ADD COLUMN local_counterparty_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
   ADD COLUMN counterparty_version BIGINT UNSIGNED NULL,
   ADD COLUMN category_source VARCHAR(16) NULL,
   ADD COLUMN integration_policy VARCHAR(24) NULL;
 ELSEIF column_count<>5 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='contract local source columns incomplete';
 END IF;
 SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index) INTO index_columns
 FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='con_contract'
 AND index_name='idx_contract_local_counterparty';
 IF index_columns IS NULL THEN
  ALTER TABLE con_contract ADD KEY idx_contract_local_counterparty(tenant_id,local_counterparty_id);
 ELSEIF index_columns<>'tenant_id,local_counterparty_id' THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='contract local counterparty index differs';
 END IF;
END;
CALL con_expand_local_catalog();
DROP PROCEDURE con_expand_local_catalog;

CREATE TABLE IF NOT EXISTS con_local_counterparty (
 tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 name VARCHAR(255) NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT TRUE,
 version BIGINT UNSIGNED NOT NULL,
 owner_identity_id VARCHAR(128) NOT NULL,
 owner_org_id VARCHAR(64) NOT NULL DEFAULT '',
 owner_project_id VARCHAR(64) NOT NULL DEFAULT '',
 created_by VARCHAR(128) NOT NULL,
 updated_at DATETIME(3) NOT NULL,
 PRIMARY KEY(tenant_id,id),
 KEY idx_local_counterparty_scope(tenant_id,owner_identity_id,owner_org_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS con_local_category (
 tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 name VARCHAR(120) NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT TRUE,
 version BIGINT UNSIGNED NOT NULL,
 owner_identity_id VARCHAR(128) NOT NULL,
 owner_org_id VARCHAR(64) NOT NULL DEFAULT '',
 owner_project_id VARCHAR(64) NOT NULL DEFAULT '',
 created_by VARCHAR(128) NOT NULL,
 updated_at DATETIME(3) NOT NULL,
 PRIMARY KEY(tenant_id,id),
 UNIQUE KEY uk_local_category_code(tenant_id,code),
 KEY idx_local_category_scope(tenant_id,owner_identity_id,owner_org_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
