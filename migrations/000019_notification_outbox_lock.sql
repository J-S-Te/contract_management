-- 合同通知 outbox 增加投递租约时间：投递 worker 领取时写入，超时后可被回收重投。
ALTER TABLE con_notification_outbox ADD COLUMN locked_at DATETIME(3) NULL AFTER attempts;
