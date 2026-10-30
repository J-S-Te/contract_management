-- 文档类型和历史来源只扩展现行模型；原BLOB及旧审批快照不复制、不改写。
-- DDL已提交但迁移记账前中断时，重跑先核完整列组及索引，不吞部分结构错误。
DROP PROCEDURE IF EXISTS con_expand_document_source;
-- con_expand_document_source 用于可恢复地扩展源文件和历史来源列。
-- 参数：无，作用域为当前已选中的合同数据库。
-- 返回值：无；列组不完整或索引定义漂移时显式中止。
CREATE PROCEDURE con_expand_document_source()
BEGIN
    DECLARE column_count INT DEFAULT 0;
    DECLARE index_columns TEXT DEFAULT '';
    DECLARE index_unique INT DEFAULT 1;
    SELECT COUNT(*) INTO column_count FROM information_schema.columns
    WHERE table_schema = DATABASE() AND table_name = 'con_contract'
      AND column_name IN ('document_format','source_media_type','source_filename',
        'source_sha256','external_paper_number','origin_kind','import_mode',
        'origin_import_item_id','historical_status','historical_customer_name',
        'historical_owner_label','import_policy_json');
    IF column_count = 0 THEN
        ALTER TABLE con_contract
          ADD COLUMN document_format VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NULL,
          ADD COLUMN source_media_type VARCHAR(128) NULL,
          ADD COLUMN source_filename VARCHAR(255) NULL,
          ADD COLUMN source_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
          ADD COLUMN external_paper_number VARCHAR(128) NULL,
          ADD COLUMN origin_kind VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'NORMAL',
          ADD COLUMN import_mode VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL,
          ADD COLUMN origin_import_item_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
          ADD COLUMN historical_status VARCHAR(32) NULL,
          ADD COLUMN historical_customer_name VARCHAR(255) NULL,
          ADD COLUMN historical_owner_label VARCHAR(128) NULL,
          ADD COLUMN import_policy_json JSON NULL;
    ELSEIF column_count <> 12 THEN
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'contract document-source columns incomplete; inspect schema before retry';
    END IF;
    SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index), MAX(non_unique)
      INTO index_columns, index_unique FROM information_schema.statistics
      WHERE table_schema = DATABASE() AND table_name = 'con_contract'
        AND index_name = 'uk_con_contract_import_item';
    IF index_columns IS NULL THEN
        ALTER TABLE con_contract ADD UNIQUE KEY uk_con_contract_import_item (tenant_id,origin_import_item_id);
    ELSEIF index_columns <> 'tenant_id,origin_import_item_id' OR index_unique <> 0 THEN
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'contract import-item unique index differs from expected definition';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.statistics WHERE table_schema=DATABASE()
      AND table_name='con_contract' AND index_name='idx_con_contract_source_sha') THEN
        ALTER TABLE con_contract ADD KEY idx_con_contract_source_sha (tenant_id,source_sha256);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.statistics WHERE table_schema=DATABASE()
      AND table_name='con_contract' AND index_name='idx_con_contract_paper_number') THEN
        ALTER TABLE con_contract ADD KEY idx_con_contract_paper_number (tenant_id,external_paper_number);
    END IF;
END;
CALL con_expand_document_source();
DROP PROCEDURE con_expand_document_source;
