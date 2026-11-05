-- 历史合同导入自动开立 CRM 客户/商机的幂等台账：只新增本方状态记录，
-- 不回填既有导入批次、合同、目录或任何冻结摘要。
DROP PROCEDURE IF EXISTS con_create_import_crm_provision;
-- con_create_import_crm_provision 幂等建表并失败关闭校验列集合、主键与索引列序。
-- 参数：无，作用域为当前合同数据库。
-- 返回值：无；表结构缺失、列数不符或索引列序不同时以 SQLSTATE 45000 失败。
CREATE PROCEDURE con_create_import_crm_provision()
BEGIN
 DECLARE table_count INT DEFAULT 0;
 DECLARE table_column_count INT DEFAULT 0;
 DECLARE column_count INT DEFAULT 0;
 DECLARE index_columns TEXT DEFAULT '';
 SELECT COUNT(*) INTO table_count FROM information_schema.tables
  WHERE table_schema=DATABASE() AND table_name='con_import_crm_provision';
 IF table_count=0 THEN
  CREATE TABLE con_import_crm_provision (
   tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
   source_system VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
   external_record_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
   batch_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
   customer_id BIGINT UNSIGNED NULL,
   customer_created TINYINT(1) NOT NULL DEFAULT 0,
   opportunity_id BIGINT UNSIGNED NULL,
   opportunity_created TINYINT(1) NOT NULL DEFAULT 0,
   state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
   attempt INT NOT NULL DEFAULT 0,
   last_error_code VARCHAR(64) NULL,
   last_error_message VARCHAR(512) NULL,
   created_at DATETIME(6) NOT NULL,
   updated_at DATETIME(6) NOT NULL,
   PRIMARY KEY(tenant_id,source_system,external_record_id),
   KEY idx_import_crm_provision_state(state),
   KEY idx_import_crm_provision_batch(tenant_id,batch_id),
   CONSTRAINT chk_import_crm_provision_state CHECK (state IN ('PENDING','CUSTOMER_READY','CONTRACT_READY','OPPORTUNITY_READY','COMPLETED','BLOCKED'))
  ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
 END IF;
 SELECT COUNT(*) INTO table_column_count FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='con_import_crm_provision';
 SELECT COUNT(*) INTO column_count FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='con_import_crm_provision'
  AND column_name IN ('tenant_id','source_system','external_record_id','batch_id','customer_id','customer_created',
   'opportunity_id','opportunity_created','state','attempt','last_error_code','last_error_message','created_at','updated_at');
 IF table_column_count<>14 OR column_count<>14 THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='contract import CRM provision columns differ';
 END IF;
 SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index) INTO index_columns
  FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='con_import_crm_provision'
  AND index_name='PRIMARY';
 IF index_columns IS NULL OR index_columns<>'tenant_id,source_system,external_record_id' THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='contract import CRM provision primary key differs';
 END IF;
 SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index) INTO index_columns
  FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='con_import_crm_provision'
  AND index_name='idx_import_crm_provision_state';
 IF index_columns IS NULL OR index_columns<>'state' THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='contract import CRM provision state index differs';
 END IF;
 SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index) INTO index_columns
  FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='con_import_crm_provision'
  AND index_name='idx_import_crm_provision_batch';
 IF index_columns IS NULL OR index_columns<>'tenant_id,batch_id' THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='contract import CRM provision batch index differs';
 END IF;
END;
CALL con_create_import_crm_provision();
DROP PROCEDURE con_create_import_crm_provision;
