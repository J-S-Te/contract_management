-- 模板元数据与源文件分开更新：元数据保存不得回写整份旧模板，
-- 并发替换 DOCX 时用版本号做乐观校验，避免旧请求覆盖新正文/网关指针。
ALTER TABLE con_contract_template
    ADD COLUMN version BIGINT UNSIGNED NOT NULL DEFAULT 1 AFTER file_gateway_last_error;
