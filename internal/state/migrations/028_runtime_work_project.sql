-- NTM State Store: scope runtime_work by project
-- Version: 028
-- Issue: bd-o3oq3
--
-- runtime_work was keyed by bead_id alone, so one shared state.db held a
-- single work projection for every project: a refresh in one checkout
-- replaced the rows another checkout had collected. Rows are now keyed by
-- (project_dir, bead_id), where project_dir is the project root the work
-- was collected from. runtime_work_snapshots (024) is a separate table,
-- already keyed by project_dir, and is left as it is.
--
-- The rebuild keeps every column runtime_work had at 027: the 007 columns
-- plus title_disclosure from 010.
--
-- The table is a cache the next refresh refills, and rows collected before
-- this migration name no project, so the rebuild drops them rather than
-- copying them under a project no reader asks for. The migration runner
-- wraps each file in a transaction.

DROP TABLE IF EXISTS runtime_work;

CREATE TABLE runtime_work (
    project_dir TEXT NOT NULL,
    bead_id TEXT NOT NULL,
    title TEXT NOT NULL,
    title_disclosure TEXT,
    status TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 2,
    bead_type TEXT NOT NULL DEFAULT 'task',
    assignee TEXT,
    claimed_at TIMESTAMP,
    blocked_by_count INTEGER NOT NULL DEFAULT 0,
    unblocks_count INTEGER NOT NULL DEFAULT 0,
    labels TEXT,
    score REAL,
    score_reason TEXT,
    collected_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    stale_after TIMESTAMP NOT NULL,
    PRIMARY KEY (project_dir, bead_id)
);

CREATE INDEX IF NOT EXISTS idx_runtime_work_status ON runtime_work(status);
CREATE INDEX IF NOT EXISTS idx_runtime_work_priority ON runtime_work(priority);
CREATE INDEX IF NOT EXISTS idx_runtime_work_assignee ON runtime_work(assignee);
CREATE INDEX IF NOT EXISTS idx_runtime_work_score ON runtime_work(score);
