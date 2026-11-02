-- 系统级合同编号规则（全局单行）与按 (年度, 规则指纹) 分桶的全系统流水；
-- 存量合同编号不回填、不重算。
CREATE TABLE IF NOT EXISTS con_contract_number_rule (
  id TINYINT UNSIGNED NOT NULL,
  prefix VARCHAR(32) NOT NULL DEFAULT '',
  year_segment VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'YYYY',
  seq_width TINYINT UNSIGNED NOT NULL DEFAULT 4,
  suffix VARCHAR(32) NOT NULL DEFAULT '',
  updated_by VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 默认规则保持既有可读性；管理员可在界面改为 GTSR- 等前缀。
INSERT INTO con_contract_number_rule (id, prefix, year_segment, seq_width, suffix, updated_by)
VALUES (1, 'HT-', 'YYYY', 4, '', 'migration')
ON DUPLICATE KEY UPDATE id = id;

-- 流水是全系统一个年度计数器：不按租户拆分，保证单号跨租户也不重号。
-- rule_key 是规则内容（前缀、年份段、位宽、后缀）的稳定指纹，规则改了就换桶，
-- 因此改规则后当年流水自然从 1 重新开始；改回原样则回到原桶继续递增。
CREATE TABLE IF NOT EXISTS con_contract_number_sequence (
  seq_year SMALLINT UNSIGNED NOT NULL,
  rule_key VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  current_value INT UNSIGNED NOT NULL DEFAULT 0,
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (seq_year, rule_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
