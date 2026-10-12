-- +goose Up
CREATE TABLE session_merges (                   -- one merge job per completed recording
  id           TEXT PRIMARY KEY,
  session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  status       TEXT NOT NULL CHECK (status IN ('pending','submitted','succeeded','failed')),
  job_id       TEXT,                            -- smartclass-merge job id once submitted
  dest_bucket  TEXT,
  dest_key     TEXT,
  size_bytes   BIGINT,
  sha256       TEXT,
  attempts     INTEGER NOT NULL DEFAULT 0,      -- submission attempts
  last_error   TEXT,
  next_poll_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  claimed_at   TIMESTAMPTZ,                     -- claim lease, like session_photos
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_session_merges_session ON session_merges(session_id);
CREATE INDEX idx_session_merges_due ON session_merges(next_poll_at)
  WHERE status IN ('pending','submitted');

-- +goose Down
DROP TABLE IF EXISTS session_merges;
