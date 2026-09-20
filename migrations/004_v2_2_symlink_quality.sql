ALTER TABLE code_index_builds
    ADD COLUMN symlinks_skipped INT NOT NULL DEFAULT 0;
