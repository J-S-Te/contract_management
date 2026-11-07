-- 历史导入自动审批的租户级运行开关：默认关闭，只允许合同系统超级管理员读写。
-- env 能力开关（CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL）关闭时本表不参与生效判定；
-- 开启后仍须本表 enabled=1 才自动确认，version 用于乐观并发与审计核对。
CREATE TABLE IF NOT EXISTS con_import_auto_approve_setting (
  tenant_id VARCHAR(64) NOT NULL,
  enabled TINYINT(1) NOT NULL DEFAULT 0,
  version BIGINT UNSIGNED NOT NULL DEFAULT 1,
  updated_by VARCHAR(64) NOT NULL DEFAULT '',
  updated_at DATETIME(6) NOT NULL,
  PRIMARY KEY(tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
