---
title: Operations
outline: 2
---

# Operations

This page explains what the scheduler does on its own, how sessions end up `missed` or `failed`,
how a completed recording becomes a playable video, what upstream outages look like from the
outside, and how to triage each of them.

## Scheduler topology

One scheduler is the normal deployment. Extra replicas are safe but idle: leadership is elected
with `pg_try_advisory_lock(0x6469737061746368)`, so exactly one replica runs the tick at a time and
the others wait. There is no active/standby promotion test yet, so treat "one replica plus idle
spares" as the supported topology.

The tick runs every `DISPATCH_SCHED_TICK` (default `30s`) and does six things.

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

### 6. Merge

When a recording ends, dispatchub asks **smartclass-merge** to turn its segments into one playable
video and tracks that job to a terminal state. The phase is enabled only when `DISPATCH_MERGE_URL`
is set; an empty value disables merging entirely (nothing is enqueued, nothing is polled).

A merge row is enqueued on every **successful completion**, on both completion paths - the explicit
stop path and the watchdog path (`completeSession`). Failed sessions are never merged, and a session
whose `stream_id` is nil or empty is skipped. `session_merges` is unique per session, so a duplicate
enqueue is a no-op rather than an error, and a failed enqueue is logged without ever failing the
session.

Each pass claims the due rows (`status IN ('pending','submitted')`, `next_poll_at <= now`, with the
same kind of claim lease as the photo ledger) and works one job per row:

- **`pending`** (row created, job not submitted): load the session, read the stream metadata from
  webcam-server (`GET /api/streams/{stream_id}/`) as a best effort - on a metadata error the fps
  stays `0` and the codec empty, letting merge decide - and build the destination key
  `merged/<room_code>/<starts_at in DISPATCH_TIMEZONE as YYYY-MM-DD>/<session_id>.mp4`. Then submit
  with `X-Idempotency-Key: <session_id>`, so a retry after an unknown outcome replays merge's
  original job instead of creating a second one. The request carries `device_id` + `stream_id` and
  `dest.key`; it never carries `segments` (merge lists the segment prefix itself) or `dest.bucket`
  (merge's own default owns the destination bucket).
  - success → `submitted`, storing merge's job id and `next_poll_at = now + interval`;
  - `400 invalid_request` or `403 insufficient_permissions` → `failed` (permanent - the same body
    would be rejected again);
  - a `vp8`/`vp9` codec in the stream metadata → `failed` without calling merge, which rejects those
    codecs because they have no raw-bitstream demuxer;
  - anything else (a transport failure, `503 source_unavailable` while the segments are not listable
    yet, `503 service_unavailable`) → deferred and retried on the next poll; after 5 submission
    attempts the row is `failed`.
- **`submitted`** (job id stored): poll `GET /api/v1/merges/{job_id}`.
  - `succeeded` → `succeeded`, persisting the destination bucket, key, size and SHA-256 in
    `session_merges` (storage coordinates are internal - the artifacts response exposes a
    `download_url` instead, see below);
  - `failed`/`canceled` → `failed` with merge's error text;
  - `queued`/`running` → deferred to the next poll;
  - `404 job_not_found` → `failed`; a transport error or a `5xx` → deferred.

Both states are guarded by `DISPATCH_MERGE_POLL_TTL` (default `2h`): once a row is older than that
budget it is `failed` with `merge did not finish within <ttl>`, so a job that never reaches a
terminal state leaves a visible row instead of polling forever.

A merge failure **never** changes the session's own status - the recording still ends `completed`
and the merge is visible only on its own row. The poll cadence is `DISPATCH_MERGE_POLL_INTERVAL`
(default `5s`) and the outbound HTTP timeout is `DISPATCH_MERGE_TIMEOUT` (default `10s`).

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

The `session_merges` row has its own smaller machine, driven by tick phase 6 and independent of the
session status above:

| From | To | Trigger |
| --- | --- | --- |
| — | `pending` | A session completes (stop path or watchdog); `stream_id` is set and `DISPATCH_MERGE_URL` is non-empty. |
| `pending` | `submitted` | `POST /api/v1/merges` returns `201`; the job id is stored. |
| `pending` | `pending` | Retryable transport/`503` failure with submission attempts left; `next_poll_at` pushed out. |
| `pending` | `failed` | `400 invalid_request`, `403 insufficient_permissions`, an unmuxable `vp8`/`vp9` codec, or 5 submission attempts exhausted. |
| `submitted` | `succeeded` | merge reports `succeeded`; `dest_bucket`/`dest_key`/`size_bytes`/`sha256` stored on the row (never exposed by the API). |
| `submitted` | `failed` | merge reports `failed`/`canceled`, `404 job_not_found`, or the `DISPATCH_MERGE_POLL_TTL` budget expires. |
| `submitted` | `submitted` | merge still `queued`/`running`, or a transport/`5xx` poll failure. |
| `pending`/`submitted` | `failed` | TTL guard: `now - created_at > DISPATCH_MERGE_POLL_TTL`. |

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

### A merge is stuck or `failed`

The merge job lives on its own row; the session itself stays `completed`. Read it from the artifacts
endpoint (`GET /api/v1/sessions/{id}/artifacts`, the `merge` object - the session detail does not
carry it) and take the `job_id` from there when you need to look upstream:

```http
GET <DISPATCH_MERGE_URL>/api/v1/merges/{job_id}
```

`pending` rows are waiting on a submit (or retrying one), `submitted` rows are polling merge.

The API never reveals the object's storage coordinates, so an operator locates the merged video by
the merge **job id** and filehouse, not by a bucket/key pair read off the artifacts response: take
the `job_id` from the `merge` object, ask merge (`GET <DISPATCH_MERGE_URL>/api/v1/merges/{job_id}`)
for its `dest.bucket`/`dest.key`, then find the object in filehouse. dispatchub keeps the same
coordinates in its own `session_merges` row (it needs them to mint the download URL), so a database
inspection finds them too - but that is an operator-side lookup, never part of the API contract.

| Symptom | Cause | Action |
| --- | --- | --- |
| `failed` with `codec vp8 cannot be merged` (or `vp9`) | The recording's codec has no raw-bitstream demuxer, so merge rejects it permanently. | Nothing to retry; the failure is structural. Re-record for a supported codec. |
| `failed` with a `400`/`403` detail | A malformed create request, or dispatchub's credential lacks the `merge:*` keys. | Check the `merge` grants on the service credential (see [Permissions](/guide/permissions#service-credential)). |
| `failed` with `job_not_found` | merge no longer knows the job id (its database was reset, or the job aged out). | Re-enqueue is not automatic; the row is terminal. |
| `failed` with `merge did not finish within <ttl>` | The job never reached a terminal state inside its `DISPATCH_MERGE_POLL_TTL` budget (default `2h`). | Check merge's queue and `GET <DISPATCH_MERGE_URL>/readyz`; a stuck job may be cancelled upstream. |
| Stuck in `pending`, `attempts` climbing | merge is unreachable, or it keeps answering `503 source_unavailable` because the segment prefix is not listable yet. | Confirm merge is up and that webcam-server flushed segments to the source bucket. The submit retries until 5 attempts are used, then the row fails. |
| `last_error` populated but `status` still non-terminal | A retryable defer after a transport error or a `5xx`. | Wait one `DISPATCH_MERGE_POLL_INTERVAL`; the next tick retries automatically. |
| `status` is `succeeded` but the response has no `download_url` | `DISPATCH_FILEHOUSE_URL` is unset, or filehouse was unreachable (or answered `403`) when the request was handled. Minting is best effort and never fails the response. | Check `DISPATCH_FILEHOUSE_URL` and `dispatchub status`'s filehouse probe. If filehouse is up, verify the dispatchub credential carries `filehouse:share:any` + `filehouse:read:any` (see [Permissions](/guide/permissions#service-credential)) and retry the request. |

Because merging is off when `DISPATCH_MERGE_URL` is empty, a deployment without it never creates a
`session_merges` row - an absent `merge` object in the artifacts response is the expected state
there, not a failure.

### Upstream outage behaviour

- Inbound: an IAM outage answers `503` and never allows a request (fail closed). webcam-server being
  down does not take the read surface down, but control calls fail with `upstream_unavailable` and
  `/readyz` reports not ready.
- Scheduler: start retries back off, the photo poller extends `next_poll_at`, the merge phase defers
  its submit/poll to the next interval, and the watchdog keeps polling. When the upstream returns, the
  next tick resumes without a restart.
- `GET /readyz` probes Postgres and webcam-server `/readyz`, plus merge's `/readyz` when
  `DISPATCH_MERGE_URL` is set; `GET /healthz` stays 200 as long as the process lives. The filehouse
  probe lives on `status`/`doctor` instead (`checker.filehouse`), because a missing filehouse only
  suppresses `download_url` minting.

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
  until a metrics pipeline exists. The merge phase follows the same convention:
  `merges_enqueued_total`, `merges_succeeded_total`, `merges_failed_total` and
  `merge_resolution_seconds`, carried by the phase-6 log records (`action`, `session_id`, `job_id`,
  `http_status`, `upstream_detail`).
- `dispatchub status` prints the redacted config plus database version and webcam reachability, and -
  when `DISPATCH_MERGE_URL` is set - merge reachability (`checker.merge`, `GET /readyz`), plus
  filehouse reachability when `DISPATCH_FILEHOUSE_URL` is set (`checker.filehouse`, `GET /readyz`);
  `dispatchub doctor` runs the same checks with a 30 s timeout and exits non-zero on failure.

## Known gaps

These are accepted for this phase, not bugs to file against dispatchub:

- **No live preview.** webcam-server exposes no MJPEG/HLS/RTSP endpoint; frames flow
  device → accumulator → object storage only.
- **No playback from dispatchub.** The remux/container gap is closed: smartclass-merge turns the raw
  length-prefixed segments into a single playable container (mp4/mov) and uploads it to filehouse,
  and dispatchub triggers that merge when a recording completes (tick phase 6). dispatchub still
  stores no video and serves no streaming or range path: it mints a presigned `download_url` for the
  merged object through filehouse and hands that out on the artifacts response, so a player follows
  the URL rather than a byte stream from dispatchub. When `DISPATCH_FILEHOUSE_URL` is unset the
  response carries no URL at all and the object must be fetched from filehouse out of band.
- **No event feed.** Everything upstream is polled (photos, watchdog); there is no webhook or
  callback.
- **Teacher identity.** `teacher_user_id` is resolved lazily and may stay `NULL` until a
  username-to-subject lookup is confirmed in teamusers.
