-- Realistic OpenCode v1 (pre-sqlite-migration) storage fixture.
-- DDL taken verbatim from ~/.local/share/opencode/opencode.db.
CREATE TABLE `session` (
          `id` text PRIMARY KEY,
          `project_id` text NOT NULL,
          `workspace_id` text,
          `parent_id` text,
          `slug` text NOT NULL,
          `directory` text NOT NULL,
          `path` text,
          `title` text NOT NULL,
          `version` text NOT NULL,
          `share_url` text,
          `summary_additions` integer,
          `summary_deletions` integer,
          `summary_files` integer,
          `summary_diffs` text,
          `metadata` text,
          `cost` real DEFAULT 0 NOT NULL,
          `tokens_input` integer DEFAULT 0 NOT NULL,
          `tokens_output` integer DEFAULT 0 NOT NULL,
          `tokens_reasoning` integer DEFAULT 0 NOT NULL,
          `tokens_cache_read` integer DEFAULT 0 NOT NULL,
          `tokens_cache_write` integer DEFAULT 0 NOT NULL,
          `revert` text,
          `permission` text,
          `agent` text,
          `model` text,
          `time_created` integer NOT NULL,
          `time_updated` integer NOT NULL,
          `time_compacting` integer,
          `time_archived` integer
        );
CREATE INDEX `session_parent_idx` ON `session` (`parent_id`);
CREATE TABLE `message` (
          `id` text PRIMARY KEY,
          `session_id` text NOT NULL,
          `time_created` integer NOT NULL,
          `time_updated` integer NOT NULL,
          `data` text NOT NULL
        );
CREATE INDEX `message_session_time_created_id_idx` ON `message` (`session_id`,`time_created`,`id`);
CREATE TABLE `session_message` (
          `id` text PRIMARY KEY,
          `session_id` text NOT NULL,
          `type` text NOT NULL,
          `seq` integer NOT NULL,
          `time_created` integer NOT NULL,
          `time_updated` integer NOT NULL,
          `data` text NOT NULL
        );
CREATE UNIQUE INDEX `session_message_session_seq_idx` ON `session_message` (`session_id`,`seq`);

INSERT INTO session (id,project_id,parent_id,slug,directory,title,version,cost,tokens_input,tokens_output,time_created,time_updated) VALUES
 ('ses_main001','proj_1',NULL,'refactor-parser','/Users/dev/proj','Refactor the parser pipeline','1.2.3',0.0163,1500,280,1742300000000,1742300600000),
 ('ses_sub001','proj_1','ses_main001','explore-tokens','/Users/dev/proj','Explore token mapping','1.2.3',0.004,300,40,1742300100000,1742300300000),
 ('ses_other001','proj_1',NULL,'sqlite-reader','/Users/dev/proj2','','1.2.3',0.0,100,10,1742301000000,1742301200000),
 ('ses_empty001','proj_1',NULL,'empty-session','/Users/dev/proj2','Never used','1.2.3',0.0,0,0,1742301300000,1742301300000);

INSERT INTO message (id,session_id,time_created,time_updated,data) VALUES
 ('msg_a1','ses_main001',1742300010000,1742300010500,'{"parentID":"msg_u1","role":"assistant","mode":"build","agent":"build","variant":"max","path":{"cwd":"/Users/dev/proj","root":"/Users/dev/proj"},"cost":0.0123,"tokens":{"total":1750,"input":1000,"output":200,"reasoning":50,"cache":{"write":100,"read":400}},"modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742300010000,"completed":1742300010500},"finish":"stop"}'),
 ('msg_u1','ses_main001',1742300005000,1742300005000,'{"role":"user","time":{"created":1742300005000},"agent":"build","model":{"providerID":"anthropic","modelID":"claude-sonnet-4-5","variant":"max"},"summary":{"diffs":[]}}'),
 ('msg_a2','ses_main001',1742300020000,1742300020000,'{"role":"assistant","agent":"build","path":{"cwd":"/Users/dev/proj","root":"/Users/dev/proj"},"cost":0,"tokens":{"total":580,"input":500,"output":80,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742300020000,"completed":1742300020000},"finish":"tool-calls"}'),
 ('msg_notok','ses_main001',1742300025000,1742300025000,'{"role":"assistant","agent":"build","modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742300025000}}'),
 ('msg_sub1','ses_sub001',1742300110000,1742300110000,'{"role":"assistant","agent":"explore","path":{"cwd":"/Users/dev/proj","root":"/Users/dev/proj"},"cost":0.004,"tokens":{"total":450,"input":300,"output":40,"reasoning":10,"cache":{"write":0,"read":100}},"modelID":"gpt-5","providerID":"openai","time":{"created":1742300110000,"completed":1742300110000},"finish":"stop"}'),
 ('msg_dual','ses_other001',1742301010000,1742301010000,'{"role":"assistant","agent":"build","path":{"root":"/Users/dev/proj2"},"cost":0,"tokens":{"total":110,"input":100,"output":10,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"glm-5.2","providerID":"rn-copy","time":{"created":1742301010000}}'),
 ('msg_zero','ses_main001',1742300027000,1742300027000,'{"role":"assistant","agent":"build","cost":0,"tokens":{"total":0,"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"claude-sonnet-4-5","providerID":"anthropic","time":{"created":1742300027000},"error":{"name":"MessageAbortedError"}}');

INSERT INTO session_message (id,session_id,type,seq,time_created,time_updated,data) VALUES
 ('msg_sw1','ses_main001','agent-switched',0,1742300030000,1742300030000,'{"type":"agent-switched","agent":"build"}'),
 ('msg_dual','ses_other001','assistant',1,1742301010000,1742301050000,'{"role":"assistant","agent":"build","cost":0,"tokens":{"total":170,"input":150,"output":20,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"glm-5.2","providerID":"rn-copy","time":{"created":1742301010000}}');
