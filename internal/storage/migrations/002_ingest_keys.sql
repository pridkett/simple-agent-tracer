CREATE TABLE ingest_keys (
 id TEXT PRIMARY KEY,
 project TEXT NOT NULL REFERENCES projects(id),
 source TEXT NOT NULL,
 prefix TEXT NOT NULL,
 digest TEXT NOT NULL UNIQUE,
 created BIGINT NOT NULL
);
CREATE INDEX ingest_keys_project ON ingest_keys(project, created, id);
