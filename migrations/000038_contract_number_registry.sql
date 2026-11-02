-- 全系统合同编号登记表：一个单号在全系统只能签发一次，跨租户不重号由主键强制，
-- 不再只依赖 con_contract_number_sequence 恰好连续递增。
-- 流水桶与登记表各司其职：流水只负责给出候选号（可能因改规则换桶、或因流水表丢失而
-- 从 1 重新开始），登记表主键负责最终裁决，候选号已被占用就继续顺延。
--
-- 职责分工（不要混为一谈）：
--   1) 「存量有没有重号」由预检 fail-closed 保证：migration.Run 在应用本迁移的任何语句
--      之前会自动检测 con_contract 的重复编号——只按 contract_number 原值分组，直接继承
--      该列 utf8mb4_unicode_ci 排序规则，与登记表主键语义等价（大小写、尾随空格视为相同）。
--      发现重号时迁移中止、本迁移不产生任何写入，错误信息逐条列出重复编号与涉及租户
--      （最多 20 组，更多请用 cmd/contract-number-audit 查看完整清单），并给出清理指引。
--      因此原先「运维必须先手工跑两条 SQL」的步骤已取消。
--   2) 「重跑能不能续跑」由下面的回填语句负责：回填跳过已登记编号，进程在回填提交后、
--      con_schema_migration 记账前被中断也不会让重跑逐行报 1062。续跑只跳过“已经登记过
--      的同一个编号”，不会掩盖真正的重号——真重号在预检阶段就已经被拦下了。
-- 下面两条查询仅作为人工排查参考，日常部署无需手工执行（口径同预检，勿改成 UPPER(TRIM)）：
--   1) 跨租户重号（同一 contract_number 落在多个租户或同租户多份合同）：
--      SELECT contract_number, COUNT(*) c, GROUP_CONCAT(tenant_id) FROM con_contract
--      WHERE contract_number IS NOT NULL AND contract_number <> ''
--      GROUP BY contract_number HAVING c > 1;
--   2) 登记表已有、但合同行仍缺登记的历史缺口（用 cmd/contract-number-audit 亦可）：
--      SELECT c.contract_number, c.tenant_id FROM con_contract c
--      LEFT JOIN con_contract_number_registry r ON r.contract_number = c.contract_number
--      WHERE c.contract_number IS NOT NULL AND c.contract_number <> ''
--        AND r.contract_number IS NULL;
-- 结果行必须按业务口径清理或受控更正后再迁移；本文件不回退、不静默吞掉任何重复。
--
-- 登记表只做唯一性登记，不是合同主数据：编号更正、作废与审计口径仍以 con_contract 为准。
CREATE TABLE IF NOT EXISTS con_contract_number_registry (
  contract_number VARCHAR(64) NOT NULL,
  tenant_id       VARCHAR(64) NOT NULL,
  contract_id     VARCHAR(32) NOT NULL,
  seq_year        SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  rule_key        VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  issued_at       DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (contract_number),
  KEY idx_con_contract_number_registry_tenant (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 回填存量编号：WHERE NOT EXISTS 让本语句可续跑（已登记的编号直接跳过），
-- 但仍是一条普通 INSERT ... SELECT，故意不加 INSERT IGNORE / ON DUPLICATE KEY UPDATE：
-- 真重号会在这条语句上以 1062 失败，逼迫上线前先完成数据清理，而不是让登记表带着一个
-- 随机胜出的租户悄悄上线。fail-closed 由上面的预检前置保证，这里只负责“中断后可重跑”。
-- MySQL 8.0+ 允许 INSERT ... SELECT 的 WHERE 子查询读目标表本身（SELECT 部分先求值），
-- 8.4 已验证通过；若未来换数据库再按该方言调整。
-- seq_year/rule_key 留默认值：存量编号由旧规则或模板格式生成，无法反推规则桶，
-- 记为“历史签发”，不参与任何流水推理。
INSERT INTO con_contract_number_registry (contract_number, tenant_id, contract_id)
SELECT c.contract_number, c.tenant_id, c.id FROM con_contract AS c
WHERE c.contract_number IS NOT NULL AND c.contract_number <> ''
  AND NOT EXISTS (SELECT 1 FROM con_contract_number_registry AS r WHERE r.contract_number = c.contract_number);
