-- OpenCode v2 storage fixture: the session_message generation with the newer
-- session_v2 table and no v1 `message`/`session` tables at all.
CREATE TABLE `session_v2` (
          `id` text PRIMARY KEY,
          `parent_id` text,
          `directory` text NOT NULL,
          `title` text NOT NULL,
          `time_created` integer NOT NULL,
          `time_updated` integer NOT NULL
        );
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

INSERT INTO session_v2 (id,parent_id,directory,title,time_created,time_updated) VALUES
 ('ses_v2main','','/Users/dev/proj3','Wire up the sqlite reader',1742304000000,1742304600000),
 ('ses_v2sub','ses_v2main','/Users/dev/proj3','Check the migration list',1742304100000,1742304200000);

INSERT INTO session_message (id,session_id,type,seq,time_created,time_updated,data) VALUES
 ('msg_v2a','ses_v2main','assistant',0,1742304010000,1742304010000,'{"role":"assistant","agent":"build","cost":0.007,"tokens":{"total":900,"input":700,"output":120,"reasoning":30,"cache":{"write":50,"read":0}},"modelID":"kimi-k2","providerID":"moonshot","time":{"created":1742304010000}}'),
 ('msg_v2sw','ses_v2main','model-switched',1,1742304020000,1742304020000,'{"type":"model-switched","modelID":"kimi-k2"}'),
 ('msg_v2b','ses_v2sub','assistant',0,1742304110000,1742304110000,'{"role":"assistant","agent":"explore","cost":0,"tokens":{"total":260,"input":240,"output":20,"reasoning":0,"cache":{"write":0,"read":0}},"modelID":"kimi-k2","providerID":"moonshot","time":{"created":1742304110000}}');
