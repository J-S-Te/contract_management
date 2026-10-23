CREATE TABLE IF NOT EXISTS con_contract_payment_term (
  id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  contract_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  installment_no INT UNSIGNED NOT NULL,
  due_date DATE NOT NULL,
  amount_minor BIGINT NOT NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_con_payment_term_installment (tenant_id, contract_id, installment_no),
  KEY idx_con_payment_term_contract (tenant_id, contract_id),
  CONSTRAINT fk_con_payment_term_contract FOREIGN KEY (contract_id) REFERENCES con_contract(id),
  CONSTRAINT ck_con_payment_term_amount CHECK (amount_minor > 0),
  CONSTRAINT ck_con_payment_term_installment CHECK (installment_no > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
