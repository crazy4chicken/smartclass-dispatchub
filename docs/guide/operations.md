---
title: Operations
outline: 2
---

# Operations

This page explains what the scheduler does on its own, how sessions end up `missed` or `failed`,
what upstream outages look like from the outside, and how to triage each of them.

## Scheduler topology

One scheduler is the normal deployment. Extra replicas are safe but idle: leadership is elected
with `pg_try_advisory_lock(0x6469737061746368)`, so exactly one replica runs the tick at a time and
the others wait. There is no active/standby promotion test yet, so treat "one replica plus idle
spares" as the supported topology.

The tick runs every `DISPATCH_SCHED_TICK` (default `30s`) and does five things.

### 1. Claim

```sql
SELECT ... FROM sessions
 WHERE status = 'planned'
   AND starts_at <= now() + PRESTART
   AND ends_at > now()
   FOR UPDATE SKIP LOCKED
```

Each claimed session moves to `starting`. A session whose window closed before it could be claimed
never starts (`ends_at > now()` is part of the claim).

### 2. Start

`POST /api/devices/{device_id}/recording/start {"camera_enum": N}` against webcam-server:

- On `201` the returned `stream_id`, `started_at` and `status='recording'` are stored. The
  `stream_id` is the durable handle everything else is derived from.
- On `409 already streaming` dispatchub **adopts** the active upstream stream
  (`GET /api/devices/{device_id}/streams`, `status='active'` for that camera) instead of failing.
  This is the recovery path for streams left `active` by a webcam-server crash.
- On `502`/timeout the start is retried with exponential backoff up to
  `DISPATCH_START_RETRY_MAX` (default `3`) attempts, then the session is marked `failed` with
  `last_error` holding the upstream detail.

All outbound work passes through a bounded worker pool capped by
`DISPATCH_MAX_CONCURRENT_COMMANDS` (default `8`) - never one goroutine per session.

### 3. Stop

Sessions still `recording` when `ends_at + DISPATCH_POSTSTOP_GRACE` has passed (default grace `0s`)
move to `stopping`, then issue the upstream stop. `stopped_at` is taken from the returned
`ended_at`, and the session becomes `completed`. A `404 no active stream` is **not** an error: it
means upstream already finished, so dispatchub re-reads the stream row for the stored `stream_id`
and copies its terminal status.

### 4. Watchdog

webcam-server sends no events (see the risk list), so every tick also polls
`GET /api/streams/{stream_id}/` for sessions in `recording`/`stopping` that are older than one tick.
If upstream reports `completed`/`failed` - or the stream is gone (`404`) - the session mirrors that
status and stops being retried. This is what turns "webcam-server was killed mid-recording" into a
`failed` session instead of one stuck in `recording` forever.

### 5. Housekeeping

A `planned` session past `starts_at + DISPATCH_MISS_GRACE` (default `10m`) becomes `missed` and
logs a WARN. `missed` sessions are the operator's attention list:

```http
GET /api/v1/sessions?status=missed
```

### State transitions

| From | To | Trigger |
| --- | --- | --- |
| `planned` | `starting` | `starts_at - DISPATCH_PRESTART` reached, claimed on a tick. |
| `starting` | `recording` | Upstream `201`; `stream_id` stored. |
| `starting` | `starting` | Retryable upstream/transport failure, retries left. |
| `starting` | `failed` | Retries exhausted or a non-retryable `4xx`. |
| `planned` | `missed` | `starts_at + DISPATCH_MISS_GRACE` passed. |
| `planned` | `canceled` | A `replace`-mode import superseded the entry. |
| `recording` | `stopping` | `ends_at + DISPATCH_POSTSTOP_GRACE` passed. |
| `stopping` | `completed` | Upstream stop returns the completed stream. |
| `recording` | `failed` | Watchdog sees the upstream stream `failed`/`completed` or `404`. |

## Triage

### A lesson was not recorded (`missed`)

1. `GET /api/v1/sessions?status=missed` lists the casualties, with `room_code` and `starts_at`.
2. `GET /api/v1/rooms/{room_code}/live` shows whether the device is online now; `GET /api/v1/sessions/{id}`
   carries the last known state.
3. The usual causes:
   - the device was offline when the prestart window opened (cameras booted late) - the start
     retries, then the session is missed;
   - an orphaned upstream `active` stream from a previous crash - adopt-on-409 handles it, but if the
     orphan holds a camera the fresh start cannot disarm it, stop it once from the webcam-server API
     and the next tick recovers;
   - the term's `week1_monday`, a period time, or `DISPATCH_TIMEZONE` is wrong, which schedules the
     whole term at the wrong instants. Fix the term/periods and re-import: the import is idempotent
     and `replace` cancels the superseded rows' future sessions.

### A session is `failed`

`GET /api/v1/sessions/{id}` exposes `retry_count` and `last_error`. Map the error to a cause:

| `last_error` / detail | Likely cause |
| --- | --- |
| `device_offline` | The device dropped off webcam-server. Check the device registration and power. |
| `upstream_unavailable` | webcam-server unreachable or 502; check `/readyz` of both services. |
| `permission denied` from webcam-server | dispatchub's credential is missing `cam:control:any` / `cam:read:any`; see [Permissions](/guide/permissions#service-credential). |
| `already_streaming` after retries | An orphaned stream blocks the camera; stop it upstream. |

### Photos stay `pending` or become `unresolved`

The photo id is minted server-side and recovered by polling; there is no webhook (accepted design
cost). A row that passes `DISPATCH_PHOTO_POLL_TTL` becomes `unresolved` and keeps retrying with a
growing delay, so lag is expected during a webcam-server outage and clears when it recovers.

1. Confirm the capture reached webcam-server: `GET /api/devices/{device_id}/photos` upstream should
   contain the `request_id` returned by `POST /api/v1/rooms/{room_code}/photo`.
2. If it is absent, the command never produced a photo - check `camera_commands` for the audit row
   and `GET /api/rooms/{room_code}/live` for device state.
3. Photos produced without a `request_id` upstream are never claimed by dispatchub.

### Upstream outage behaviour

- Inbound: an IAM outage answers `503` and never allows a request (fail closed). webcam-server being
  down does not take the read surface down, but control calls fail with `upstream_unavailable` and
  `/readyz` reports not ready.
- Scheduler: start retries back off, the photo poller extends `next_poll_at`, and the watchdog keeps
  polling. When the upstream returns, the next tick resumes without a restart.
- `GET /readyz` probes Postgres and webcam-server `/readyz`; `GET /healthz` stays 200 as long as
  the process lives.

### Room/permission drift

`rooms.team_id`/`owner_id` are snapshots taken when the binding was created. If a device is re-owned
in webcam-server, the room's scope resolution keeps using the stale snapshot. Re-bind the room
(`PUT /api/v1/rooms/{room_code}`) to refresh it - the binding is the source of truth for scopes.

## Observability

- Logs are `log/slog` JSON on stdout at `DISPATCH_LOG_LEVEL`. Every outbound call logs
  `{room_code, session_id, action, http_status, duration_ms, upstream_detail}`. Tokens and
  `download_url`s never appear in logs.
- Counters are log-derived (the fleet has no metrics stack today): `sessions_started_total`,
  `sessions_failed_total`, `sessions_missed_total`, `photo_resolution_seconds` - grep the JSON logs
  until a metrics pipeline exists.
- `dispatchub status` prints the redacted config plus database version and webcam reachability;
  `dispatchub doctor` runs the same checks with a 30 s timeout and exits non-zero on failure.

## Known gaps

These are accepted for this phase, not bugs to file against dispatchub:

- **No live preview.** webcam-server exposes no MJPEG/HLS/RTSP endpoint; frames flow
  device → accumulator → object storage only.
- **No playback.** Recorded segments are raw length-prefixed chunks with no container and no
  recorded codec; a decoder/remux step does not exist yet, so dispatchub can hand out segment URLs
  but nothing plays them end-to-end.
- **No event feed.** Everything upstream is polled (photos, watchdog); there is no webhook or
  callback.
- **Teacher identity.** `teacher_user_id` is resolved lazily and may stay `NULL` until a
  username-to-subject lookup is confirmed in teamusers.
