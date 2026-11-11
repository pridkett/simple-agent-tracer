CREATE TABLE projects (
 id TEXT PRIMARY KEY,
 slug TEXT NOT NULL UNIQUE,
 retention_days INTEGER NOT NULL CHECK (retention_days > 0),
 capture BOOLEAN NOT NULL
);
CREATE TABLE sources (
 project TEXT NOT NULL REFERENCES projects(id),
 id TEXT NOT NULL,
 PRIMARY KEY (project, id)
);
CREATE TABLE traces (
 project TEXT NOT NULL REFERENCES projects(id),
 id TEXT NOT NULL,
 latest BIGINT NOT NULL,
 received BIGINT NOT NULL,
 kept BOOLEAN NOT NULL DEFAULT FALSE,
 PRIMARY KEY (project, id)
);
CREATE INDEX traces_order ON traces(project, latest DESC, id DESC);
CREATE INDEX traces_retention ON traces(project, kept, latest, received);
CREATE TABLE sessions (
 project TEXT NOT NULL REFERENCES projects(id),
 id TEXT NOT NULL,
 source TEXT NOT NULL,
 external TEXT NOT NULL,
 latest BIGINT NOT NULL DEFAULT 0,
 kept BOOLEAN NOT NULL DEFAULT FALSE,
 PRIMARY KEY (project, id),
 FOREIGN KEY (project, source) REFERENCES sources(project, id)
);
CREATE INDEX sessions_order ON sessions(project, latest DESC, id DESC);
CREATE INDEX session_external ON sessions(project, external);
CREATE TABLE spans (
 project TEXT NOT NULL,
 trace TEXT NOT NULL,
 id TEXT NOT NULL,
 parent TEXT NOT NULL,
 source TEXT NOT NULL,
 service TEXT NOT NULL,
 status TEXT NOT NULL,
 operation TEXT NOT NULL,
 request_model TEXT NOT NULL,
 response_model TEXT NOT NULL,
 name TEXT NOT NULL,
 tool TEXT NOT NULL,
 proxy_app TEXT NOT NULL,
 application TEXT NOT NULL,
 span_kind TEXT NOT NULL,
 own_session TEXT,
 own_turn TEXT NOT NULL,
 session_id TEXT,
 turn_id TEXT NOT NULL DEFAULT '',
 turn_from_proxy BOOLEAN NOT NULL DEFAULT FALSE,
 background BOOLEAN NOT NULL DEFAULT FALSE,
 linked_model BOOLEAN NOT NULL DEFAULT FALSE,
 usage_client TEXT NOT NULL DEFAULT '',
 model_call BOOLEAN NOT NULL,
 sort_time BIGINT NOT NULL,
 activity BIGINT NOT NULL,
 conflicts BIGINT NOT NULL DEFAULT 0,
 usage TEXT NOT NULL,
 payload TEXT NOT NULL,
 PRIMARY KEY (project, trace, id),
 FOREIGN KEY (project, trace) REFERENCES traces(project, id) ON DELETE CASCADE,
 FOREIGN KEY (project, source) REFERENCES sources(project, id),
 FOREIGN KEY (project, own_session) REFERENCES sessions(project, id),
 FOREIGN KEY (project, session_id) REFERENCES sessions(project, id)
);
CREATE INDEX span_trace_order ON spans(project, trace, sort_time, id);
CREATE INDEX span_session_order ON spans(project, session_id, sort_time, trace, id);
CREATE INDEX span_session_activity ON spans(project, session_id, activity DESC);
CREATE INDEX span_own_session ON spans(project, own_session);
CREATE TABLE span_metadata (
 project TEXT NOT NULL,
 trace TEXT NOT NULL,
 span TEXT NOT NULL,
 location TEXT NOT NULL,
 key TEXT NOT NULL,
 type TEXT NOT NULL,
 digest TEXT NOT NULL,
 value TEXT NOT NULL,
 PRIMARY KEY (project, trace, span, location, key),
 FOREIGN KEY (project, trace, span) REFERENCES spans(project, trace, id) ON DELETE CASCADE
);
CREATE INDEX metadata_filter ON span_metadata(project, key, type, digest, trace, span);
CREATE TABLE session_claims (
 project TEXT NOT NULL,
 trace TEXT NOT NULL,
 proxy_session TEXT NOT NULL,
 agent_session TEXT NOT NULL,
 PRIMARY KEY (project, trace, proxy_session, agent_session),
 FOREIGN KEY (project, proxy_session) REFERENCES sessions(project, id),
 FOREIGN KEY (project, agent_session) REFERENCES sessions(project, id)
);
CREATE INDEX claims_proxy ON session_claims(project, proxy_session, agent_session);
CREATE INDEX claims_agent ON session_claims(project, agent_session);
CREATE TABLE session_aliases (
 project TEXT NOT NULL,
 proxy_session TEXT NOT NULL,
 agent_session TEXT NOT NULL,
 PRIMARY KEY (project, proxy_session),
 FOREIGN KEY (project, proxy_session) REFERENCES sessions(project, id) ON DELETE CASCADE,
 FOREIGN KEY (project, agent_session) REFERENCES sessions(project, id) ON DELETE CASCADE
);
CREATE INDEX aliases_agent ON session_aliases(project, agent_session, proxy_session);
