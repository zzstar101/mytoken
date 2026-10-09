-- OpenClaw agent store at step 2: beta-store-1 gains a second assistant turn
-- without a provider-recorded cost, and a second session (beta-store-2)
-- appears in the same store. seq is the store-wide row counter, so the new
-- session starts at 6.
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
  (3, 'beta-store-1', '{"type":"message","timestamp":"2026-03-02T12:00:02Z","message":{"role":"user","content":[{"type":"text","text":"Summarise the store migration"}]}}', NULL, 1772452802000),
  (4, 'beta-store-1', '{"type":"message","id":"beta-store-1-call-a","timestamp":"2026-03-02T12:00:07Z","message":{"role":"assistant","content":[{"type":"text","text":"SENTINEL_PRIVATE_TEXT sits in a transcript row whose content the parser never reports."}],"usage":{"input":1200,"output":260,"cacheRead":300,"cacheWrite":80,"cost":{"total":0.042}}}}', NULL, 1772452807000),
  (5, 'beta-store-1', '{"type":"message","id":"beta-store-1-call-b","timestamp":"2026-03-02T12:01:07Z","message":{"role":"assistant","content":[{"type":"text","text":"Follow-up without a provider-recorded cost."}],"usage":{"input":500,"output":90,"cacheRead":0,"cacheWrite":0}}}', NULL, 1772452867000),
  (6, 'beta-store-2', '{"type":"session","id":"beta-store-2","timestamp":"2026-03-02T14:00:00Z","provider":"openai"}', NULL, 1772460000000),
  (7, 'beta-store-2', '{"type":"custom","customType":"model-snapshot","timestamp":"2026-03-02T14:00:01Z","data":{"modelId":"gpt-5-mini"}}', NULL, 1772460001000),
  (8, 'beta-store-2', '{"type":"message","id":"beta-store-2-call-a","timestamp":"2026-03-02T14:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"Second session in the same store."}],"usage":{"input":800,"output":120,"cacheRead":40,"cacheWrite":0,"cost":{"total":0.0077}}}}', NULL, 1772460004000);
