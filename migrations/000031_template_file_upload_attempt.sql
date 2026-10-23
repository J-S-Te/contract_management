-- New upload keys are only allocated after resolving a terminal FAILED request
-- and acknowledging its exact binding release. Preserve all failed identities.
ALTER TABLE con_template_file_outbox
    ADD COLUMN upload_attempt BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER attempts,
    ADD COLUMN upload_failures_json JSON NULL AFTER upload_attempt;
