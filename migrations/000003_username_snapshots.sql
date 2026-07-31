-- Keep stable platform user IDs as relational keys while storing the login username that was
-- current when a contract or approval event was created. Usernames are presentation snapshots;
-- authorization and ownership checks must continue to use the immutable user IDs.

ALTER TABLE con_contract
    ADD COLUMN owner_username VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT ''
        AFTER owner_user_id;

ALTER TABLE con_approval_instance
    ADD COLUMN applicant_username VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT ''
        AFTER applicant_user_id;

ALTER TABLE con_approval_action
    ADD COLUMN actor_username VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT ''
        AFTER actor_user_id;
