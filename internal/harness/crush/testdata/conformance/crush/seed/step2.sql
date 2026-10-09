-- Step 2: a nested session that recorded usage, a session that never ran a
-- message (Crush skips it entirely) and a late message on the unused session.
INSERT INTO sessions (id,parent_session_id,title,message_count,prompt_tokens,completion_tokens,cost,created_at,updated_at) VALUES
 ('sess-c-summarizer','sess-b-child-retry','Summarize the migration',3,640,90,0.0051,1772366400000,1772366405000),
 ('sess-y-never-used',NULL,'Never used',0,0,0,0.0,1772370000000,1772370000000);
INSERT INTO messages (id,session_id,role,parts,model,provider,is_summary_message,created_at,updated_at) VALUES
 ('msg-c1','sess-c-summarizer','assistant','[]','claude-haiku-4-5','anthropic',0,1772366400000,1772366400000),
 ('msg-z2','sess-z-scratch','assistant','[]','claude-sonnet-4-5','anthropic',0,1772360401000,1772360401000);
