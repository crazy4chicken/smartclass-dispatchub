-- +goose Up
CREATE TABLE terms (
  term_code    TEXT PRIMARY KEY,               -- e.g. 2026-FALL
  name         TEXT NOT NULL,
  week1_monday DATE NOT NULL,                  -- Monday of teaching week 1
  weeks        INTEGER NOT NULL CHECK (weeks BETWEEN 1 AND 30),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE periods (                          -- 节次时间表, per term
  term_code  TEXT NOT NULL REFERENCES terms(term_code) ON DELETE CASCADE,
  period_no  INTEGER NOT NULL CHECK (period_no BETWEEN 1 AND 20),
  start_time TIME NOT NULL,
  end_time   TIME NOT NULL,
  PRIMARY KEY (term_code, period_no),
  CHECK (end_time > start_time)
);

CREATE TABLE rooms (                            -- room_code -> webcam-server device binding
  room_code   TEXT PRIMARY KEY,
  name        TEXT NOT NULL DEFAULT '',
  device_id   TEXT NOT NULL,                    -- webcam-server device ULID (opaque here)
  camera_enum INTEGER NOT NULL DEFAULT 0,
  team_id     TEXT,                             -- snapshotted from the device
  owner_id    TEXT,
  enabled     BOOLEAN NOT NULL DEFAULT true,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (device_id, camera_enum)
);

CREATE TABLE imports (                          -- one row per CSV upload
  id          TEXT PRIMARY KEY,
  term_code   TEXT NOT NULL REFERENCES terms(term_code),
  mode        TEXT NOT NULL CHECK (mode IN ('replace','append')),
  filename    TEXT NOT NULL,
  sha256      TEXT NOT NULL,
  row_count   INTEGER NOT NULL DEFAULT 0,
  ok_count    INTEGER NOT NULL DEFAULT 0,
  error_count INTEGER NOT NULL DEFAULT 0,
  errors      JSONB NOT NULL DEFAULT '[]',      -- [{row,column,code,message}]
  status      TEXT NOT NULL CHECK (status IN ('dry_run','committed','failed')),
  imported_by TEXT NOT NULL,                    -- claims.subject
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE schedule_entries (                 -- normalized CSV rows
  id               TEXT PRIMARY KEY,
  import_id        TEXT NOT NULL REFERENCES imports(id) ON DELETE CASCADE,
  term_code        TEXT NOT NULL REFERENCES terms(term_code),
  superseded_by    TEXT REFERENCES imports(id), -- set when a later replace-import wins
  course_code      TEXT NOT NULL,
  course_name      TEXT NOT NULL,
  teacher_username TEXT NOT NULL,
  teacher_user_id  TEXT,                        -- resolved lazily (see §5)
  room_code        TEXT NOT NULL REFERENCES rooms(room_code),
  weekday          SMALLINT NOT NULL CHECK (weekday BETWEEN 1 AND 7),
  period_start     INTEGER NOT NULL,
  period_end       INTEGER NOT NULL,
  weeks            TEXT NOT NULL,               -- spec, e.g. "1-16", "1,3,5-9"
  raw              JSONB NOT NULL DEFAULT '{}',
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (period_end >= period_start)
);

CREATE TABLE sessions (                         -- one recording occurrence (scheduled or ad hoc)
  id          TEXT PRIMARY KEY,
  entry_id    TEXT REFERENCES schedule_entries(id) ON DELETE CASCADE,  -- NULL for manual
  room_code   TEXT NOT NULL REFERENCES rooms(room_code),
  device_id   TEXT NOT NULL,
  camera_enum INTEGER NOT NULL,
  starts_at   TIMESTAMPTZ NOT NULL,
  ends_at     TIMESTAMPTZ NOT NULL,
  status      TEXT NOT NULL CHECK (status IN
                ('planned','starting','recording','stopping','completed','failed','canceled','missed')),
  origin      TEXT NOT NULL CHECK (origin IN ('schedule','manual')),
  stream_id   TEXT,                             -- webcam-server stream id, THE durable handle
  started_at  TIMESTAMPTZ,
  stopped_at  TIMESTAMPTZ,
  retry_count INTEGER NOT NULL DEFAULT 0,
  last_error  TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (ends_at > starts_at)
);
CREATE UNIQUE INDEX idx_sessions_entry_start ON sessions(entry_id, starts_at) WHERE origin = 'schedule';
CREATE UNIQUE INDEX idx_sessions_live_stream ON sessions(stream_id)      WHERE stream_id IS NOT NULL;
CREATE INDEX idx_sessions_due       ON sessions(starts_at) WHERE status = 'planned';
CREATE INDEX idx_sessions_recording ON sessions(ends_at)   WHERE status = 'recording';
CREATE INDEX idx_sessions_room_time ON sessions(room_code, starts_at DESC);

CREATE TABLE session_photos (                   -- photo id resolution ledger
  id           TEXT PRIMARY KEY,
  session_id   TEXT REFERENCES sessions(id) ON DELETE SET NULL,
  room_code    TEXT NOT NULL REFERENCES rooms(room_code),
  device_id    TEXT NOT NULL,
  camera_enum  INTEGER NOT NULL,
  request_id   TEXT NOT NULL,                   -- from POST …/photo
  photo_id     TEXT,                            -- webcam-server photo id, resolved by polling
  source       TEXT NOT NULL CHECK (source IN ('scheduled','manual')),
  actor_id     TEXT,
  status       TEXT NOT NULL CHECK (status IN ('pending','resolved','unresolved')),
  attempts     INTEGER NOT NULL DEFAULT 0,
  next_poll_at TIMESTAMPTZ,
  taken_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  resolved_at  TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_photos_pending  ON session_photos(next_poll_at) WHERE status = 'pending';
CREATE UNIQUE INDEX idx_photos_id ON session_photos(photo_id)    WHERE photo_id IS NOT NULL;

CREATE TABLE camera_commands (                  -- audit + idempotency for mid-session control
  id              TEXT PRIMARY KEY,
  session_id      TEXT REFERENCES sessions(id) ON DELETE SET NULL,
  room_code       TEXT NOT NULL,
  device_id       TEXT NOT NULL,
  action          TEXT NOT NULL CHECK (action IN ('start','stop','switch_camera','photo')),
  camera_enum     INTEGER NOT NULL,
  actor_id        TEXT NOT NULL,
  idempotency_key TEXT,
  request_id      TEXT,
  command_id      TEXT,
  outcome         TEXT NOT NULL CHECK (outcome IN ('accepted','rejected','failed')),
  http_status     INTEGER,
  detail          TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_camera_commands_idem ON camera_commands(idempotency_key)
  WHERE idempotency_key IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS camera_commands;
DROP TABLE IF EXISTS session_photos;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS schedule_entries;
DROP TABLE IF EXISTS imports;
DROP TABLE IF EXISTS rooms;
DROP TABLE IF EXISTS periods;
DROP TABLE IF EXISTS terms;
