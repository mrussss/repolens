-- Serialized report payloads are bounded by the application to 4 MiB per field.
-- TEXT cannot hold even a three-citation report. Preserve bounded scalar TEXT
-- fields and the existing MEDIUMTEXT full-report/checkpoint columns.
ALTER TABLE reports
    MODIFY COLUMN findings_json MEDIUMTEXT NOT NULL,
    MODIFY COLUMN recommended_checks_json MEDIUMTEXT,
    MODIFY COLUMN limitations_json MEDIUMTEXT;
