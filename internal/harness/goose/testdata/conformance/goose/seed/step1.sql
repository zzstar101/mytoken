-- Step 1: the notes session finished its turn, so Goose updates its running
-- totals and its last-active time in place.
UPDATE sessions
   SET updated_at = '2026-03-03 11:00:20',
       accumulated_input_tokens = 800,
       accumulated_output_tokens = 210
 WHERE id = 'goose-bravo-notes';

INSERT INTO messages (message_id, session_id, role, content_json, created_timestamp) VALUES
  ('bravo-m3', 'goose-bravo-notes', 'assistant',
   '[{"type":"text","text":"The migration notes are in the pull request."}]', 1772449220000);
