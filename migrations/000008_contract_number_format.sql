ALTER TABLE con_contract_template
    ADD COLUMN number_format VARCHAR(160) NOT NULL DEFAULT 'HT-{YYYYMMDD}-{ID8}' AFTER original_filename;

ALTER TABLE con_contract
    ADD COLUMN contract_number_format VARCHAR(160) NOT NULL DEFAULT 'HT-{YYYYMMDD}-{ID8}' AFTER contract_number;
