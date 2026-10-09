-- Pinned Bernard v0.5.1 datastore/sqlite/migrations/1_init.sql followed by
-- datastore/sqlite/migrations/2_no_foreign_keys.sql, in dependency order.
-- The handoff test uses this fixture so it tests cursor/persistence behavior
-- independently of the upstream migrator's unordered map iteration.

PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS file (
    "id" text NOT NULL,
    "drive" text NOT NULL,
    "name" text NOT NULL,
    "parent" text NOT NULL,
    "size" integer NOT NULL,
    "md5" text NOT NULL,
    "trashed" boolean NOT NULL,
    PRIMARY KEY(id, drive),
    FOREIGN KEY(parent, drive) REFERENCES folder(id, drive) DEFERRABLE INITIALLY IMMEDIATE
);

CREATE TABLE IF NOT EXISTS folder (
    "id" text NOT NULL,
    "drive" text NOT NULL,
    "name" text NOT NULL,
    "trashed" boolean NOT NULL,
    "parent" text,
    PRIMARY KEY(id, drive),
    FOREIGN KEY(parent, drive) REFERENCES folder(id, drive) DEFERRABLE INITIALLY IMMEDIATE
);

CREATE TABLE IF NOT EXISTS drive (
    "id" text NOT NULL,
    "pageToken" text NOT NULL,
    PRIMARY KEY(id)
);

CREATE TABLE IF NOT EXISTS file_new (
    "id" text NOT NULL,
    "drive" text NOT NULL,
    "name" text NOT NULL,
    "parent" text NOT NULL,
    "size" integer NOT NULL,
    "md5" text NOT NULL,
    "trashed" boolean NOT NULL,
    PRIMARY KEY(id, drive)
);

CREATE TABLE IF NOT EXISTS folder_new (
    "id" text NOT NULL,
    "drive" text NOT NULL,
    "name" text NOT NULL,
    "trashed" boolean NOT NULL,
    "parent" text,
    PRIMARY KEY(id, drive)
);

INSERT INTO file_new SELECT * FROM file;
INSERT INTO folder_new SELECT * FROM folder;

DROP TABLE file;
DROP TABLE folder;

ALTER TABLE file_new RENAME TO file;
ALTER TABLE folder_new RENAME TO folder;
