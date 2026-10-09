-- Step 0: a root session that recorded usage and a session that never did.
-- Crush stores timestamps in milliseconds when they are large enough and in
-- seconds otherwise, so the unused session is written in seconds.
INSERT INTO sessions (id,parent_session_id,title,message_count,prompt_tokens,completion_tokens,cost,created_at,updated_at) VALUES
 ('sess-a-root-cache',NULL,'Refactor the widget cache',6,1200,300,0.031,1772359200000,1772359210000),
 ('sess-z-scratch',NULL,'Scratch notes',2,0,0,0.0,1772360400,1772360400);
INSERT INTO messages (id,session_id,role,parts,model,provider,is_summary_message,created_at,updated_at) VALUES
 ('msg-a1','sess-a-root-cache','user','[]','','',0,1772359200000,1772359200000),
 ('msg-a2','sess-a-root-cache','assistant','[{"type":"text","text":"Rebuilt the widget cache and dropped the stale entries. SENTINEL_PRIVATE_TEXT"}]','claude-sonnet-4-5','anthropic',0,1772359201000,1772359202000),
 ('msg-a3','sess-a-root-cache','assistant','[]','','',0,1772359205000,1772359205000),
 ('msg-z1','sess-z-scratch','user','[]','','',0,1772360400000,1772360400000);
