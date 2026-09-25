-- The migration runner applies this resumably and backfills existing builds.
ALTER TABLE code_index_builds ADD COLUMN build_tags_json TEXT NULL;
UPDATE code_index_builds SET build_tags_json = '[]' WHERE build_tags_json IS NULL OR build_tags_json = '';
-- Pending builds with non-empty tag hashes cannot be reconstructed because
-- prior versions stored only the hash. Fail those jobs closed. READY rows stay
-- immutable historical artifacts; v2.2 parser/pipeline identities prevent
-- them from being reused for new preparations.
UPDATE code_index_builds
SET status = 'FAILED', error_code = 'BUILD_TAGS_UNAVAILABLE'
WHERE status IN ('CREATED', 'BUILDING')
  AND (build_tags_hash IS NULL OR build_tags_hash <> 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855')
  AND COALESCE(error_code, '') <> 'BUILD_TAGS_UNAVAILABLE';
