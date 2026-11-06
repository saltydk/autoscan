-- Queue identities that have been configured. `active` marks identities
-- configured on the most recent run, which distinguishes a target whose
-- identity just changed (URL or rewrite edit) from one removed earlier.
-- `adopted_from` records the queue an identity took over, so reverting the
-- change returns the backlog to the original identity.
CREATE TABLE target_identity (
    id TEXT PRIMARY KEY NOT NULL,
    active INTEGER NOT NULL DEFAULT 0,
    adopted_from TEXT
);
