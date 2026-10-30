-- 历史确认、受控责任激活及数析同轮水位；不回填或清理业务数据。
DROP PROCEDURE IF EXISTS con_expand_import_history;
-- con_expand_import_history 扩展历史发布列并检测中断产生的部分结构。
-- 参数：无，使用当前合同schema。
-- 返回值：无；不完整结构失败关闭。
CREATE PROCEDURE con_expand_import_history()
BEGIN
 DECLARE n INT DEFAULT 0;
 SELECT COUNT(*) INTO n FROM information_schema.columns WHERE table_schema=DATABASE()
 AND table_name='con_contract' AND column_name IN ('import_source_revision',
 'import_report_release_version','import_report_required_channels','declared_signed_date',
 'import_confirmed_at','import_responsibility_activated_at');
 IF n=0 THEN
 ALTER TABLE con_contract
 ADD import_source_revision BIGINT UNSIGNED NOT NULL DEFAULT 0,
 ADD import_report_release_version BIGINT UNSIGNED NOT NULL DEFAULT 0,
 ADD import_report_required_channels VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
 ADD declared_signed_date DATE NULL,
 ADD import_confirmed_at DATETIME(6) NULL,
 ADD import_responsibility_activated_at DATETIME(6) NULL;
 ELSEIF n<>6 THEN
 SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='contract import history columns incomplete';
 END IF;
END;
CALL con_expand_import_history();
DROP PROCEDURE con_expand_import_history;
