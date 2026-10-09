-- Schema of the Goose session database, as far as MyToken reads it.
CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  working_dir TEXT,
  created_at TEXT,
  updated_at TEXT,
  accumulated_input_tokens INTEGER NOT NULL DEFAULT 0,
  accumulated_output_tokens INTEGER NOT NULL DEFAULT 0,
  provider_name TEXT,
  model_config_json BLOB
);

CREATE TABLE messages (
  message_id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  role TEXT NOT NULL,
  content_json BLOB NOT NULL,
  created_timestamp INTEGER NOT NULL
);
