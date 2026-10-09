-- Crush per-project database fixture (internal/db/migrations schema).
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
-- project A: one root session (timestamps in SECONDS), one child subagent
-- session (timestamps in MILLISECONDS) and one unused session.
INSERT INTO sessions (id,parent_session_id,title,message_count,prompt_tokens,completion_tokens,cost,created_at,updated_at) VALUES
 ('s_root001',NULL,'Build the CLI',4,12000,800,0.05,1742303000,1742303600),
 ('s_child001','s_root001','Inspect the schema',2,3000,150,0.01,1742303100000,1742303200000),
 ('s_zero001',NULL,'Never used',0,0,0,0.0,1742303300,1742303300);
INSERT INTO messages (id,session_id,role,parts,model,provider,created_at,updated_at) VALUES
 ('m_root1','s_root001','assistant','[]','claude-sonnet-4-5','anthropic',1742303000,1742303100),
 ('m_child1','s_child001','assistant','[]','gpt-5','openai',1742303100000,1742303150000),
 ('m_root2','s_root001','assistant','[]','','',1742303400,1742303400),
 ('m_user1','s_root001','user','[]','','',1742303001,1742303001);
