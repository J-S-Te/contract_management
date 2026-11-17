-- 记录状态与业务状态分离；不释放合同编号，不清理原件和幂等来源。
-- 每列独立检查，允许 DDL 完成但迁移记账前中断后的安全重入。
DROP PROCEDURE IF EXISTS con_ensure_record_column;
CREATE PROCEDURE con_ensure_record_column(
 IN target_table VARCHAR(64), IN target_column VARCHAR(64),
 IN expected_type VARCHAR(32), IN expected_nullable VARCHAR(3), IN column_definition TEXT
)
BEGIN
 DECLARE found_count INT DEFAULT 0;
 DECLARE found_type VARCHAR(32) DEFAULT '';
 DECLARE found_nullable VARCHAR(3) DEFAULT '';
 SELECT COUNT(*), COALESCE(MAX(data_type),''), COALESCE(MAX(is_nullable),'')
 INTO found_count, found_type, found_nullable
 FROM information_schema.columns
 WHERE table_schema=DATABASE() AND table_name=target_table AND column_name=target_column;
 IF found_count > 0 AND (found_type <> expected_type OR found_nullable <> expected_nullable) THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='contract record lifecycle column definition mismatch';
 END IF;
 IF found_count=0 THEN
  SET @record_ddl=CONCAT('ALTER TABLE `',target_table,'` ADD COLUMN `',target_column,'` ',column_definition);
  PREPARE record_stmt FROM @record_ddl;
  EXECUTE record_stmt;
  DEALLOCATE PREPARE record_stmt;
 END IF;
END;
CALL con_ensure_record_column('con_contract','deleted_at','datetime','YES','DATETIME(3) NULL');
CALL con_ensure_record_column('con_contract','deleted_by','varchar','YES','VARCHAR(128) NULL');
CALL con_ensure_record_column('con_contract','delete_reason','varchar','YES','VARCHAR(1000) NULL');
CALL con_ensure_record_column('con_contract_lifecycle_event','event_type','varchar','NO',
 'VARCHAR(16) NOT NULL DEFAULT ''STATUS_CHANGE''');
DROP PROCEDURE con_ensure_record_column;
DROP PROCEDURE IF EXISTS con_ensure_record_index;
CREATE PROCEDURE con_ensure_record_index()
BEGIN
 IF NOT EXISTS (SELECT 1 FROM information_schema.statistics
  WHERE table_schema=DATABASE() AND table_name='con_contract'
  AND index_name='idx_contract_record_state') THEN
  ALTER TABLE con_contract ADD INDEX idx_contract_record_state (tenant_id,deleted_at,id);
 END IF;
END;
CALL con_ensure_record_index();
DROP PROCEDURE con_ensure_record_index;
