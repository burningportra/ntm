-- NTM State Store: scope runtime_coordination by project
-- Version: 029
-- Issue: bd-dh5kx
--
-- runtime_coordination was keyed by agent_name alone, so one shared state.db
-- held a single coordination projection for every project: Agent Mail names
-- repeat across projects, and a refresh in one checkout replaced or deleted
-- the rows another checkout had collected. Rows are now keyed by
-- (project_dir, agent_name), where project_dir is the project root whose
-- Agent Mail inboxes were read, the same key runtime_work uses since 028.
--
-- The rebuild keeps every column runtime_coordination had at 028: the 007
-- columns plus the four subject/preview disclosure columns from 009.
--
-- The table is a cache the next refresh refills, and rows collected before
-- this migration name no project, so the rebuild drops them rather than
-- copying them under a project no reader asks for. The migration runner
-- wraps each file in a transaction.

DROP TABLE IF EXISTS runtime_coordination;

CREATE TABLE runtime_coordination (
    project_dir TEXT NOT NULL,
    agent_name TEXT NOT NULL,
    session_name TEXT,
    pane TEXT,
    unread_count INTEGER NOT NULL DEFAULT 0,
    pending_ack_count INTEGER NOT NULL DEFAULT 0,
    urgent_count INTEGER NOT NULL DEFAULT 0,
    last_message_subject TEXT,
    last_message_subject_disclosure TEXT,
    last_message_preview TEXT,
    last_message_preview_disclosure TEXT,
    last_message_at TIMESTAMP,
    last_sent_at TIMESTAMP,
    last_received_at TIMESTAMP,
    collected_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    stale_after TIMESTAMP NOT NULL,
    PRIMARY KEY (project_dir, agent_name)
);

CREATE INDEX IF NOT EXISTS idx_runtime_coord_session ON runtime_coordination(session_name);
CREATE INDEX IF NOT EXISTS idx_runtime_coord_unread ON runtime_coordination(unread_count);
CREATE INDEX IF NOT EXISTS idx_runtime_coord_urgent ON runtime_coordination(urgent_count);
