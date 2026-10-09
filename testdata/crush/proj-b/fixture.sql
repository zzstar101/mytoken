CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  parent_session_id TEXT,
  title TEXT,
  message_count INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost REAL NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE messages (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  role TEXT NOT NULL,
  parts TEXT NOT NULL DEFAULT '[]',
  model TEXT,
  provider TEXT,
  is_summary_message INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  finished_at INTEGER
);
INSERT INTO sessions (id,parent_session_id,title,message_count,prompt_tokens,completion_tokens,cost,created_at,updated_at) VALUES
 ('s_b001',NULL,'',1,500,20,0.0,1742305000000,1742305000000);
INSERT INTO messages (id,session_id,role,parts,model,provider,created_at,updated_at) VALUES
 ('m_b1','s_b001','assistant','[]','qwen3-coder','alibaba',1742305000000,1742305000000);
