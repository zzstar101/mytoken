-- Step 0: two conversations exist. The cache conversation already carries its
-- first usage record; the notes conversation has none at all, and its title is
-- missing so the first user message supplies it. The row whose context is NULL
-- is not a session and must not be discovered. One workspace is registered, so
-- the cache conversation is labelled by its directory; the notes conversation
-- has no workspace at all (id 0) and must not come out with a project.
INSERT INTO workspaces (workspace_id, path, name) VALUES
  (4815162342, '/Users/demo/widget-cache', 'widget-cache');

INSERT INTO conversations (conversation_id, title, workspace_id, context, created_at, updated_at, metrics) VALUES
  ('conv-a-cache', 'Refactor the widget cache', 4815162342,
   '{"conversation_id":"conv-a-cache","messages":[{"message":{"text":{"role":"User","content":"Refactor   the cache\nlayer"}}},{"message":{"text":{"role":"Assistant","content":"","model":"claude-sonnet-4-5","tool_calls":[{"name":"shell","call_id":"call-a-1","arguments":{"command":"go test ./internal/cache/..."}},{"name":"Read","call_id":"call-a-2","arguments":{"file_path":"internal/cache/layer.go"}}]}},"usage":{"prompt_tokens":{"actual":1200},"completion_tokens":{"actual":300},"total_tokens":{"actual":1500},"cached_tokens":{"actual":200}}},{"message":{"text":{"role":"Assistant","content":"The probe prints SENTINEL_PRIVATE_TEXT into the trace file.","model":"claude-sonnet-4-5"}}}]}',
   '2026-03-02 10:00:00', '2026-03-02 10:05:00', NULL),
  ('conv-c-notes', NULL, 0,
   '{"conversation_id":"conv-c-notes","messages":[{"message":{"text":{"role":"User","content":"Log the release checklist"}}},{"message":{"text":{"role":"Assistant","content":"Logged, nothing to bill yet.","model":"claude-haiku-4-5"}}}]}',
   '2026-03-02 09:00:00', '2026-03-02 09:01:00', NULL),
  ('conv-d-null-context', 'Never started', 7, NULL,
   '2026-03-02 08:00:00', NULL, NULL);
