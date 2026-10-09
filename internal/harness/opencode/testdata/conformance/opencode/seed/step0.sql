-- OpenCode conformance seed, growth step 0 of 2: schema, sessions and the first
-- half of the rows. One SQL statement per line (see conformance_test.go); '--'
-- lines and blank lines are skipped. Hand-written and fully synthetic.
--
-- The v2 `session_message` row msg_c4 is deliberately older than message rows
-- written by a later growth step: the two tables resume from independent
-- watermarks, so an interleaved layout must not lose it.
CREATE TABLE session (id text PRIMARY KEY, parent_id text, directory text, title text, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL, seq integer NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
INSERT INTO session (id,parent_id,directory,title,time_created,time_updated) VALUES ('ses_conf_main',NULL,'/Users/dev/confproj','Refactor the conformance harness',1742400000000,1742400300000);
INSERT INTO session (id,parent_id,directory,title,time_created,time_updated) VALUES ('ses_conf_sub','ses_conf_main','/Users/dev/confproj','Explore the token mapping',1742400100000,1742400250000);
INSERT INTO message (id,session_id,time_created,time_updated,data) VALUES ('msg_c_u0','ses_conf_main',1742400000500,1742400000500,'{"role":"user","time":{"created":1742400000500},"text":"Refactor the conformance harness"}');
INSERT INTO message (id,session_id,time_created,time_updated,data) VALUES ('msg_c1','ses_conf_main',1742400010000,1742400010000,'{"role":"assistant","agent":"build","path":{"cwd":"/Users/dev/confproj","root":"/Users/dev/confproj"},"cost":0.0123,"tokens":{"total":1750,"input":1000,"output":200,"reasoning":50,"cache":{"write":100,"read":400}},"modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742400010000,"completed":1742400012000},"text":"SENTINEL_PRIVATE_TEXT"}');
INSERT INTO message (id,session_id,time_created,time_updated,data) VALUES ('msg_c2','ses_conf_main',1742400020000,1742400020000,'{"role":"assistant","agent":"build","modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742400020000}}');
INSERT INTO message (id,session_id,time_created,time_updated,data) VALUES ('msg_c_u1','ses_conf_sub',1742400025000,1742400025000,'{"role":"user","time":{"created":1742400025000},"text":"Explore the token mapping"}');
INSERT INTO message (id,session_id,time_created,time_updated,data) VALUES ('msg_c3','ses_conf_sub',1742400030000,1742400030000,'{"role":"assistant","agent":"explore","path":{"cwd":"/Users/dev/confproj","root":"/Users/dev/confproj"},"cost":0.004,"tokens":{"total":450,"input":300,"output":40,"reasoning":10,"cache":{"write":0,"read":100}},"modelID":"gpt-5","providerID":"openai","time":{"created":1742400030000,"completed":1742400030500}}');
INSERT INTO session_message (id,session_id,type,seq,time_created,time_updated,data) VALUES ('msg_c4','ses_conf_main','assistant',1,1742400040000,1742400040000,'{"role":"assistant","agent":"build","path":{"cwd":"/Users/dev/confproj","root":"/Users/dev/confproj"},"cost":0.002,"tokens":{"total":580,"input":500,"output":80,"reasoning":0,"cache":{"write":50,"read":0}},"modelID":"kimi-k2","providerID":"moonshot","time":{"created":1742400040000}}');
INSERT INTO message (id,session_id,time_created,time_updated,data) VALUES ('msg_c0','ses_conf_main',1742400045000,1742400045000,'{"role":"assistant","agent":"build","cost":0,"tokens":{"total":0,"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742400045000}}');
