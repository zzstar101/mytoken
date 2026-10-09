-- Step 1: the child (subagent) session and its messages arrive.
INSERT INTO sessions (id,parent_session_id,title,message_count,prompt_tokens,completion_tokens,cost,created_at,updated_at) VALUES
 ('sess-b-child-retry','sess-a-root-cache','Add the retry wrapper',4,1850,260,0.03,1772362800000,1772362815000);
INSERT INTO messages (id,session_id,role,parts,model,provider,is_summary_message,created_at,updated_at) VALUES
 ('msg-b1','sess-b-child-retry','assistant','[{"type":"text","text":"Wrapped the retry loop around the fetch."}]','gpt-5','openai',0,1772362800000,1772362800000),
 ('msg-b2','sess-b-child-retry','assistant','[{"type":"text","text":"Summary of the retry work. SENTINEL_PRIVATE_TEXT"}]','gpt-5','openai',1,1772362815000,1772362815000);
