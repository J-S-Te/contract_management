-- 已投递审计行的清理索引（启用 60 天审计清理前的索引门禁）。
--
-- internal/auditoutbox.GORMStore.PurgeDelivered 的清理谓词是
--   delivery_status = 'delivered' AND delivered_at IS NOT NULL AND delivered_at < cutoff
--   ORDER BY delivered_at ASC, id ASC LIMIT batch
-- 表上现有索引只有 000026 的领取索引 idx_con_audit_outbox_due
-- (delivery_status, next_attempt_at) 与 000040 的摘要证据索引；前导列等值命中后，
-- delivered_at 既不是范围条件也不是排序列，清理无法按索引顺序截断批次。
-- 实测（10.8 万已投递行，MySQL 8.4）：EXPLAIN 命中 idx_con_audit_outbox_due，
-- rows≈59386、Extra 为 "Using where; Using filesort"；因此每轮清理
-- （dispatcher.purgeInterval=1h）都会过滤全部已投递行并做文件排序。
--
-- 本迁移补 (delivery_status, delivered_at)：前导列等值 + 第二列范围/排序与清理谓词
-- 完全同序；InnoDB 二级索引隐式携带主键 id，ORDER BY delivered_at, id 可直接按索引
-- 顺序截断 LIMIT 批次，不再 filesort。只新增二级索引，不改列、不回填、不删除/改写
-- 任何既有行。清理默认关闭（AUDIT_OUTBOX_DELIVERED_RETENTION_DAYS 缺省=0），
-- 本索引在显式启用清理前只占空间，不改变任何读写语义。
-- 大表上 ADD KEY 会重建二级索引并短暂占用 DDL 锁，发布窗口按 DBA 约定执行。
--
-- 守卫式、可重入：本仓 runner 按版本 + checksum 记账，但 DDL 隐式提交后、记账前
-- 中断时迁移会重跑。重跑只校验既有索引定义，不重复加键、不改任何数据；
-- 同名索引若已存在但定义漂移则失败关闭，不默默替换未知索引。
DROP PROCEDURE IF EXISTS con_ensure_audit_outbox_delivered_index;
CREATE PROCEDURE con_ensure_audit_outbox_delivered_index()
BEGIN
  DECLARE index_count INT DEFAULT 0;
  DECLARE index_columns TEXT DEFAULT '';
  DECLARE index_non_unique INT DEFAULT 0;
  SELECT COUNT(*), COALESCE(GROUP_CONCAT(column_name ORDER BY seq_in_index), ''), COALESCE(MAX(non_unique), 0)
    INTO index_count, index_columns, index_non_unique
    FROM information_schema.statistics
   WHERE table_schema = DATABASE() AND table_name = 'con_audit_outbox'
     AND index_name = 'idx_con_audit_outbox_delivered';
  IF index_count > 0 AND
     (index_count <> 2 OR index_columns <> 'delivery_status,delivered_at'
      OR index_non_unique <> 1) THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT = 'con_audit_outbox delivered index definition mismatch; inspect schema before retry';
  END IF;
  IF index_count = 0 THEN
    ALTER TABLE con_audit_outbox
      ADD KEY idx_con_audit_outbox_delivered (delivery_status, delivered_at);
  END IF;
END;
CALL con_ensure_audit_outbox_delivered_index();
DROP PROCEDURE con_ensure_audit_outbox_delivered_index;
