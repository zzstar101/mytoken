-- Step 2: a third session appears complete in one go, and it is the only one
-- whose timestamps are written in RFC3339 form instead of Goose's space
-- separated form.
INSERT INTO sessions (id, name, working_dir, created_at, updated_at,
  accumulated_input_tokens, accumulated_output_tokens, provider_name, model_config_json)
VALUES
  ('goose-delta-rollout', 'auto: delta rollout', '/Users/demo/goose-delta',
   '2026-03-02T12:00:00Z', '2026-03-02T12:00:05Z', 640, 90, 'anthropic',
   '{"model_name":"claude-haiku-4-5","reasoning":true}');

INSERT INTO messages (message_id, session_id, role, content_json, created_timestamp) VALUES
  ('delta-m1', 'goose-delta-rollout', 'user',
   '[{"type":"text","text":"Draft the migration notes"}]', 1772366402000),
  ('delta-m2', 'goose-delta-rollout', 'assistant',
   '[{"type":"text","text":"The rollout checklist still carries SENTINEL_PRIVATE_TEXT."}]', 1772366405000);
