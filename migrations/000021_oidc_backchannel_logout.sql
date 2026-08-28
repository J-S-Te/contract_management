CREATE TABLE IF NOT EXISTS con_oidc_backchannel_logout_replay (
    jti_hash BINARY(32) NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (jti_hash),
    KEY idx_con_oidc_backchannel_logout_replay_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
