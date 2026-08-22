-- Supports daemon/a2a: the A2A 1.0 external-agent-interoperability
-- adapter (docs/AMH-SPECIFICATION.md §9, §14). An A2A Task maps 1:1 onto
-- an AMH Goal — the entry point of autonomous work AMH already has,
-- durably tracked and restart-survivable, not a new parallel state
-- machine invented for this adapter (§9: "MCP and A2A adapters SHALL
-- translate external lifecycle and errors into AMH canonical operation
-- states without becoming workflow authorities").
--
-- 'canceled' is a real, distinct terminal state — separate from 'failed'
-- — for A2A's CancelTask (and any future generic "stop this goal"
-- action); the original 0001 enum didn't need it because nothing before
-- this adapter offered a caller a way to cancel a goal.
ALTER TABLE goal DROP CONSTRAINT goal_status_check;
ALTER TABLE goal ADD CONSTRAINT goal_status_check CHECK (status IN ('open', 'active', 'done', 'failed', 'canceled'));

-- Human-readable text associated with the goal's current status —
-- pursue_goal's synthesized final result when done, or a reason when
-- failed/canceled. A2A's TaskStatus.message is exactly this, and no
-- existing AMH record already durably stores pursue_goal's return value
-- (DBOS itself records it, but only DBOS's own Python-SDK-accessible
-- workflow-result store, not a column daemon/a2a's Go code can read).
ALTER TABLE goal ADD COLUMN status_message TEXT;
