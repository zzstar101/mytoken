-- Step 2: a second project conversation appears complete in one go. Its
-- updated_at is NULL, so the created_at stamp (with fractional seconds) is the
-- one Forge reports, and its second usage record reports cached tokens that
-- have to be taken out of the prompt count.
INSERT INTO conversations (conversation_id, title, workspace_id, context, created_at, updated_at, metrics) VALUES
  ('conv-b-rollout', 'Summarise the store migration', 1234567890123456789,
   '{"conversation_id":"conv-b-rollout","messages":[{"message":{"text":{"role":"User","content":"Summarise the store migration"}}},{"message":{"text":{"role":"Assistant","content":"","model":"gpt-5","tool_calls":[{"name":"bash","call_id":"call-b-1","arguments":{"command":"go test ./internal/store/..."}}]}},"usage":{"prompt_tokens":{"actual":640},"completion_tokens":{"actual":90},"total_tokens":{"actual":730}}},{"message":{"text":{"role":"Assistant","content":"The migration notes quote SENTINEL_PRIVATE_TEXT once.","model":"gpt-5"}}},{"message":{"text":{"role":"Assistant","content":"The rollout checklist is in the pull request.","model":"gpt-5"}},"usage":{"prompt_tokens":{"actual":200},"completion_tokens":{"actual":40},"total_tokens":{"actual":240},"cached_tokens":{"actual":150}}}]}',
   '2026-03-03 11:00:20.379', NULL, NULL);
