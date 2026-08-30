-- Pause/resume from the dashboard (DESIGN.md §7.3).
--
-- `conductor pause` already freezes wrapped sessions locally by signaling their sidecar;
-- the dashboard needs the same reach without a terminal on the session's machine. The
-- control plane records the request on the session (pending_control), the sidecar reads
-- it from its heartbeat response — the reply it already receives and used to discard —
-- and the next heartbeat's control_ack clears it once local reality matches.

ALTER TABLE sessions
    ADD COLUMN pending_control text NOT NULL DEFAULT '';

-- A paused session is present and heartbeating but frozen mid-thought: it is not
-- waiting for its human the way waiting_for_input is.
ALTER TABLE sessions DROP CONSTRAINT sessions_state_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_state_check CHECK (state IN (
    'online_idle','planning','working','waiting_for_input','paused',
    'blocked','reviewing','offline_grace','stale','closed'));
