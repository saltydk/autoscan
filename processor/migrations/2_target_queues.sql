CREATE TABLE target_scan (
    target_id TEXT NOT NULL,
    folder TEXT NOT NULL,
    priority INTEGER NOT NULL,
    time DATETIME NOT NULL,
    generation INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (target_id, folder)
);

CREATE INDEX target_scan_order ON target_scan (target_id, priority DESC, time ASC);
