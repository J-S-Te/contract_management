-- Change snapshot columns from login usernames to Chinese display names. Existing usernames are
-- cleared so they cannot be presented as names; the UI resolves historical users from the
-- configured directory, while stable platform user IDs remain the relational identity.

ALTER TABLE con_contract
    RENAME COLUMN owner_username TO owner_display_name;

ALTER TABLE con_approval_instance
    RENAME COLUMN applicant_username TO applicant_display_name;

ALTER TABLE con_approval_action
    RENAME COLUMN actor_username TO actor_display_name;

UPDATE con_contract SET owner_display_name = '';
UPDATE con_approval_instance SET applicant_display_name = '';
UPDATE con_approval_action SET actor_display_name = '';
