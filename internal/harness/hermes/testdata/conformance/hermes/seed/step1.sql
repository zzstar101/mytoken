-- Hermes conformance seed, growth step 1 of 2: appendix only. The new session
-- (and its session_model_usage row) is added without touching anything from
-- step 0, so an incremental re-parse merges cleanly: cumulative counters are
-- snapshots keyed by <session>#<model>, and the suite compares the merged
-- snapshot with a single cold parse of the final database.
INSERT INTO sessions (id,parent_session_id,model,billing_provider,billing_base_url,cwd,git_repo_root,title,started_at,ended_at,last_activity_at,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,estimated_cost_usd,actual_cost_usd,cost_source) VALUES ('20260102_030440_01e00005','','deepseek-v4-pro','deepseek','https://api.deepseek.com','/home/user/fixture-hermes','','',1767323080.0,1767323082.0,1767323082.0,300,90,100,10,0,0,0,'none');
INSERT INTO session_model_usage (session_id,model,billing_provider,billing_base_url,task,api_call_count,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,estimated_cost_usd,actual_cost_usd,first_seen,last_seen) VALUES ('20260102_030440_01e00005','deepseek-v4-pro','deepseek','https://api.deepseek.com','',2,300,90,100,10,0,0,0,1767323080.0,1767323082.0);
INSERT INTO messages (session_id,role,content,timestamp,reasoning_content) VALUES ('20260102_030440_01e00005','user','fixture hermes: second batch growth step',1767323080.1,'');
INSERT INTO messages (session_id,role,content,timestamp,reasoning_content) VALUES ('20260102_030440_01e00005','assistant','fixture assistant reply after the growth step with SENTINEL_PRIVATE_TEXT',1767323081.0,'');
