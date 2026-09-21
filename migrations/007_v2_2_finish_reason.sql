ALTER TABLE diagnosis_attempts ADD COLUMN finish_reason VARCHAR(32) NULL;
ALTER TABLE agent_steps ADD COLUMN finish_reason VARCHAR(32) NULL;
