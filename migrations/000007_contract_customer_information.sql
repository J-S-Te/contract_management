ALTER TABLE con_contract
    MODIFY contract_number VARCHAR(64) NULL,
    ADD COLUMN opportunity_id VARCHAR(64) NULL AFTER service_type,
    ADD COLUMN opportunity_name VARCHAR(255) NULL AFTER opportunity_id,
    ADD COLUMN customer_name VARCHAR(255) NULL AFTER opportunity_name,
    ADD COLUMN customer_address VARCHAR(500) NULL AFTER customer_name,
    ADD COLUMN customer_contact VARCHAR(128) NULL AFTER customer_address,
    ADD COLUMN customer_phone VARCHAR(64) NULL AFTER customer_contact,
    ADD COLUMN systems_json JSON NULL AFTER customer_phone;
