ALTER TABLE diagnosis_runs
    ADD COLUMN max_agent_rounds INT NOT NULL DEFAULT 8,
    ADD COLUMN max_tool_calls INT NOT NULL DEFAULT 12,
    ADD COLUMN max_search_calls INT NOT NULL DEFAULT 3,
    ADD COLUMN max_repeat_calls INT NOT NULL DEFAULT 2,
    ADD COLUMN max_evidence_packet_bytes INT NOT NULL DEFAULT 32768,
    ADD COLUMN finalization_turns INT NOT NULL DEFAULT 1,
    ADD COLUMN max_output_tokens INT NOT NULL DEFAULT 2048,
    ADD COLUMN provider_timeout_seconds INT NOT NULL DEFAULT 60,
    ADD COLUMN provider_retry_attempts INT NOT NULL DEFAULT 0;

ALTER TABLE reports ADD COLUMN structured_payload_json MEDIUMTEXT NULL;
