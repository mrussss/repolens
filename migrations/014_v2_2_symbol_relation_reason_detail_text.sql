-- Related-test explanations are persisted verbatim and have no 255-character
-- application contract. Preserve complete explanations for valid long names.
ALTER TABLE symbol_relations MODIFY COLUMN reason_detail TEXT NULL;
