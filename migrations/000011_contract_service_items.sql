ALTER TABLE con_contract
    ADD COLUMN service_items_json JSON NULL AFTER systems_json;

UPDATE con_contract
SET service_items_json = JSON_ARRAY(
    JSON_OBJECT(
        'service_type', service_type,
        'systems', COALESCE(systems_json, JSON_ARRAY())
    )
)
WHERE service_items_json IS NULL;
