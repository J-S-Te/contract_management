-- 可选冻结定价与不可变发布价。存量不回填；历史缺基准始终未知。
-- DDL隐式提交后、runner记账前中断时可安全重入；既有列定义漂移失败关闭。
DROP PROCEDURE IF EXISTS con_ensure_pricing_column;
CREATE PROCEDURE con_ensure_pricing_column()
BEGIN
 DECLARE column_count INT DEFAULT 0;
 DECLARE column_type VARCHAR(32) DEFAULT '';
 DECLARE nullable_flag VARCHAR(3) DEFAULT '';
 SELECT COUNT(*), COALESCE(MAX(data_type), ''), COALESCE(MAX(is_nullable), '')
 INTO column_count, column_type, nullable_flag
 FROM information_schema.columns
 WHERE table_schema=DATABASE() AND table_name='con_contract' AND column_name='pricing_json';
 IF column_count > 0 AND (column_type <> 'json' OR nullable_flag <> 'YES') THEN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='con_contract pricing_json definition mismatch';
 END IF;
 IF column_count=0 THEN
  ALTER TABLE con_contract ADD COLUMN pricing_json JSON NULL;
 END IF;
END;
CALL con_ensure_pricing_column();
DROP PROCEDURE con_ensure_pricing_column;
CREATE TABLE IF NOT EXISTS con_standard_price (
 id VARCHAR(26) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(128) NOT NULL,
 status VARCHAR(16) NOT NULL,
 version BIGINT UNSIGNED NOT NULL,
 data_json JSON NOT NULL,
 created_at DATETIME(3) NOT NULL,
 updated_at DATETIME(3) NOT NULL,
 KEY idx_con_standard_price_tenant_status (tenant_id,status,id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE IF NOT EXISTS con_pricing_revision (
 approval_id VARCHAR(26) NOT NULL PRIMARY KEY,
 tenant_id VARCHAR(128) NOT NULL,
 contract_id VARCHAR(26) NOT NULL,
 expected_version BIGINT UNSIGNED NOT NULL,
 proposal_json JSON NOT NULL,
 created_at DATETIME(3) NOT NULL,
 KEY idx_con_pricing_revision_contract (tenant_id,contract_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
