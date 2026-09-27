-- Keep repository and snapshot refs within a single 255-character schema limit.
ALTER TABLE repositories MODIFY COLUMN default_ref VARCHAR(255) NOT NULL DEFAULT 'main';
ALTER TABLE repository_snapshots MODIFY COLUMN ref VARCHAR(255) NOT NULL;
ALTER TABLE repository_snapshots MODIFY COLUMN requested_ref VARCHAR(255) NULL;
