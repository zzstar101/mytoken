-- Step 0: the cache session already finished its turns (its running totals are
-- final and never move again), the notes session has its transcript but has not
-- completed a turn, and the scratch session has no usage at all.
INSERT INTO sessions (id, name, working_dir, created_at, updated_at,
  accumulated_input_tokens, accumulated_output_tokens, provider_name, model_config_json)
VALUES
  ('goose-alpha-cache', 'auto: alpha cache', '/Users/demo/goose-alpha',
   '2026-03-02 10:00:00', '2026-03-02 10:05:00', 1400, 320, 'anthropic',
   '{"model_name":"claude-sonnet-4-5","reasoning":true}'),
  ('goose-bravo-notes', 'auto: bravo notes', '/Users/demo/goose-bravo',
   '2026-03-03 11:00:00', '2026-03-03 11:00:00', 0, 0, 'openai',
   '{"model_name":"gpt-5","reasoning":false}'),
  ('goose-charlie-scratch', 'auto: charlie scratch', '/Users/demo/goose-scratch',
   '2026-03-02 09:00:00', '2026-03-02 09:01:00', 0, 0, 'anthropic',
   '{"model_name":"claude-haiku-4-5","reasoning":true}');

INSERT INTO messages (message_id, session_id, role, content_json, created_timestamp) VALUES
  ('alpha-m1', 'goose-alpha-cache', 'user',
   '[{"type":"text","text":"Refactor   the cache\nlayer"}]', 1772359202000),
  ('alpha-m2', 'goose-alpha-cache', 'assistant',
   '[{"type":"text","text":"Reading internal/cache/layer.go before touching anything."},{"type":"toolRequest","toolCall":{"value":{"name":"developer__read_file","arguments":{"file_path":"internal/cache/layer.go"}}}}]', 1772359209000),
  ('alpha-m3', 'goose-alpha-cache', 'assistant',
   '[{"type":"text","text":"Done - the cache layer sits behind a flag now."}]', 1772359290000),
  ('alpha-m4', 'goose-alpha-cache', 'assistant',
   '[{"type":"text","text":"The probe prints SENTINEL_PRIVATE_TEXT into the trace file."}]', 1772359380000),
  ('bravo-m1', 'goose-bravo-notes', 'user',
   '[{"type":"text","text":"Summarise the store migration"}]', 1772449205000),
  ('bravo-m2', 'goose-bravo-notes', 'assistant',
   '[{"type":"text","text":"Migration notes mention SENTINEL_PRIVATE_TEXT in passing."}]', 1772449212000);
