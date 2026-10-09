-- Step 3: one more session opens, and the cache session gains a late message.
-- The late message does not move the cache session's running totals or its
-- last-active time: Goose reports usage per turn, so a message on its own must
-- not shift the timestamp of the usage already reported.
INSERT INTO sessions (id, name, working_dir, created_at, updated_at,
  accumulated_input_tokens, accumulated_output_tokens, provider_name, model_config_json)
VALUES
  ('goose-echo-notes', 'auto: echo notes', '/Users/demo/goose-echo',
   '2026-03-03T13:00:00Z', '2026-03-03T13:00:04Z', 0, 0, 'openai',
   '{"model_name":"gpt-5","reasoning":false}');

INSERT INTO messages (message_id, session_id, role, content_json, created_timestamp) VALUES
  ('alpha-m5', 'goose-alpha-cache', 'assistant',
   '[{"type":"text","text":"SENTINEL_PRIVATE_TEXT also shows up in the late note."}]', 1772359440000),
  ('echo-m1', 'goose-echo-notes', 'user',
   '[{"type":"text","text":"Log the release checklist"}]', 1772456401000);
