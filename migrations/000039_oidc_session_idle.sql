-- 业务子系统统一空闲会话：区分显式浏览器交互（last_interactive_at）与普通已认证请求（last_seen_at）。
-- 存量行按 created_at 回填：升级后旧行的空闲基准等价于原会话创建时间，绝对到期字段不变，
-- 不在迁移中撤销或延长任何既有会话。代码侧仍把 NULL 当 created_at，兼容未回填的异常行。
ALTER TABLE con_oidc_session
    ADD COLUMN last_seen_at DATETIME(3) NULL AFTER session_expires_at,
    ADD COLUMN last_interactive_at DATETIME(3) NULL AFTER last_seen_at;

UPDATE con_oidc_session
SET last_seen_at = created_at
WHERE last_seen_at IS NULL;

UPDATE con_oidc_session
SET last_interactive_at = created_at
WHERE last_interactive_at IS NULL;
