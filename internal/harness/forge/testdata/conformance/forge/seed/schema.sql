-- The conversations table as Forge creates it.
CREATE TABLE conversations(
  conversation_id TEXT PRIMARY KEY NOT NULL,
  title TEXT,
  workspace_id BIGINT NOT NULL,
  context TEXT,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP,
  metrics TEXT
);

-- Forge's own schema is documented as the conversations table alone, so this
-- mirrors the workspaces table an installation would plausibly keep alongside
-- it: it lets the parser label a project with its directory instead of with a
-- bare numeric id.
CREATE TABLE workspaces(
  workspace_id BIGINT PRIMARY KEY NOT NULL,
  path TEXT NOT NULL,
  name TEXT
);
