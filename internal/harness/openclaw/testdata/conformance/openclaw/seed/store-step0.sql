-- OpenClaw agent store at step 0: the schema plus the first three transcript
-- rows of session beta-store-1 (the session entry, a model change and the
-- opening user message). No usage has been recorded yet.
CREATE TABLE transcript_events (
  seq        INTEGER PRIMARY KEY,
  session_id TEXT NOT NULL,
  event_json TEXT,
  event_zstd BLOB,
  created_at INTEGER
);

INSERT INTO transcript_events (seq, session_id, event_json, event_zstd, created_at) VALUES
  (1, 'beta-store-1', '{"type":"session","id":"beta-store-1","timestamp":"2026-03-02T12:00:00Z","provider":"anthropic","data":{"provider":"anthropic","modelId":"claude-sonnet-4-5"}}', NULL, 1772452800000),
  (2, 'beta-store-1', '{"type":"model_change","modelId":"claude-sonnet-4-5","timestamp":"2026-03-02T12:00:01Z"}', NULL, 1772452801000),
  (3, 'beta-store-1', '{"type":"message","timestamp":"2026-03-02T12:00:02Z","message":{"role":"user","content":[{"type":"text","text":"Summarise the store migration"}]}}', NULL, 1772452802000);
