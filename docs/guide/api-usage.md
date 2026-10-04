---
title: API Usage
outline: 2
---

# API Usage

This walkthrough follows one term from CSV upload to downloadable artifacts, then every mid-session
control call. All calls sit under `/api/v1`, take `Authorization: Bearer <teamusers access token>`
and follow the permission rules in [Permissions and access control](/guide/permissions). Lists are
wrapped in `{"items": [...]}` and errors are RFC 9457 problem details with a stable `detail` code.

Throughout, `$BASE_URL` is the service listen address (`http://127.0.0.1:8081` locally) and
`$TOKEN` is a teamusers access token.

## Import a timetable

The upload is `multipart/form-data` with the CSV part named `file`, at most 1 MiB and at most
`DISPATCH_MAX_IMPORT_ROWS` rows. Validation is always available as a dry run:

```http
POST /api/v1/timetable/imports?dry_run=true&mode=replace
Content-Type: multipart/form-data; boundary=...

file=@timetable.csv
```

`mode` is `replace` (default) or `append`. The dry run writes only the `imports` row with
`status='dry_run'` and normalizes nothing; it answers `200` with the batch plus a preview of what a
commit would materialize:

```json
{
  "id": "01J8Z9...",
  "term_code": "2026-FALL",
  "mode": "replace",
  "filename": "timetable.csv",
  "sha256": "...",
  "row_count": 200,
  "ok_count": 200,
  "error_count": 0,
  "errors": [],
  "status": "dry_run",
  "imported_by": "01J8Z8USER...",
  "preview": [
    {
      "row": 2,
      "course_code": "CS101",
      "session_count": 16,
      "first_starts_at": "2026-09-07T08:00:00+08:00",
      "last_ends_at": "2026-12-21T09:40:00+08:00"
    }
  ]
}
```

Rules the validator enforces, row by row:

- the header row is required and matched **by name in any order**; unknown or missing columns are
  rejected;
- the term exists and its `periods` cover `period_start..period_end`;
- `room_code` is bound and enabled;
- `weeks` is within `1..terms.weeks`;
- no two rows of the same import overlap on `(room_code, weekday, period range, weeks)`;
- a teacher double-booked across rooms is a **warning**: it is reported in `errors`/`error_count`
  but never rejects the file.

Any blocking row error answers `422` with the problem detail `import_validation_failed` and an
extra `errors` array carrying the blocking errors plus any warnings, and the batch is stored with
`status='failed'` and `ok_count=0` - there is no partial commit. On a successful dry run or commit
`ok_count` is the accepted row count and `error_count` counts only the warnings. Repeating the call
without `dry_run=true` commits in one transaction
and answers `201` with the import: the batch, the `schedule_entries` and the materialized
`sessions`. In `replace` mode the term's previous entries are first marked
`superseded_by = <new import id>` and their future `planned` sessions are canceled
(`status='canceled'`); sessions already `recording` or `completed` are never touched.
Concurrent imports for one term serialize on an advisory lock. Re-uploading the identical file
(same SHA-256, same term) answers `409 duplicate_import` unless you add `?force=true`.

## Sessions materialize from the rows

Each accepted row is expanded over `weeks × (weekday, period_start..period_end)` into one `sessions`
row:

```text
date(week, weekday) = week1_monday + (week - 1) * 7 + (weekday - 1) days
starts_at = date + periods[period_start].start_time   in DISPATCH_TIMEZONE
ends_at   = date + periods[period_end].end_time       in DISPATCH_TIMEZONE
```

Occurrences already in the past when the import commits are stored with `status='canceled'` (kept
for the record) instead of `planned`. A collision with an existing live **or planned** recording on
the same `(device_id, camera_enum)` is reported as the blocking row error `session_collision`, so
the whole batch fails cleanly instead of double-booking a camera. In `mode=replace` the batch's own
term is excluded from that check, because the commit supersedes its pending sessions; `mode=append`
still collides with them.

Query the result:

```http
GET /api/v1/sessions?term_code=2026-FALL&room_code=A301&status=planned
```

Filters are `term_code`, `room_code`, `date` (`YYYY-MM-DD`), `status`, `course_code` and `limit`;
results are newest `starts_at` first. `date` matches a UTC calendar day against `starts_at`
(`[00:00, next 00:00) UTC`), independent of `DISPATCH_TIMEZONE` (an unparsable date is rejected
with `400 invalid_request`), and `course_code` only matches scheduled sessions - manual sessions
have no entry to match. `limit` defaults to `100`, values above `1000` are clamped to `1000`, and a
non-numeric or non-positive value answers `400 invalid_request`. `GET /api/v1/sessions/{id}`
returns the session plus its photo ledger and the live upstream status.

## Scheduled recording, no calls needed

A `planned` session is picked up by the scheduler `DISPATCH_PRESTART` (default `2m`) before
`starts_at`. It transitions `planned → starting → recording` while the `stream_id` returned by
webcam-server is stored on the row, then `recording → stopping → completed` around `ends_at`
(plus `DISPATCH_POSTSTOP_GRACE`). If the device is flaky the start is retried up to
`DISPATCH_START_RETRY_MAX` times; if the window closes without a start, the session becomes
`missed`. See [Operations](/guide/operations) for the full semantics.

## Fetch artifacts

```http
GET /api/v1/sessions/{id}/artifacts
```

```json
{
  "stream_id": "01J8Z4W3K5M7Q9R1T3V5X7Z9B1",
  "status": "completed",
  "segments": [
    {
      "segment_seq": 1,
      "size_bytes": 73400320,
      "duration_ms": 30000,
      "download_url": "https://files.example.com/files/...?expires=..."
    }
  ],
  "photos": [
    {
      "photo_id": "01J8Z5PHOTO000000000000000",
      "taken_at": "2026-09-07T00:05:12Z",
      "download_url": "https://files.example.com/files/...?expires=..."
    }
  ]
}
```

dispatchub does not mirror segment rows: they are re-derived from the recorded `stream_id`, and the
download URLs are minted live by webcam-server with a **15-minute TTL** - never cache or persist
them. A `stream_id` with no segments yet returns the session's current upstream status with an
empty list.

## Mid-session control

All five control calls share the same envelope: they accept an optional `X-Idempotency-Key` header,
log the caller (`actor_id` = token subject) and every outcome into `camera_commands`, and translate
upstream failures into the stable error codes below. Replaying a request with the same idempotency
key returns the stored outcome instead of issuing a second upstream command.

| Call | Effect |
| --- | --- |
| `POST /api/v1/sessions/{id}/recording/start` | Manual start of a `planned` session - early start, or a re-start after `failed`. |
| `POST /api/v1/sessions/{id}/recording/stop` | Manual stop of that session. |
| `POST /api/v1/rooms/{room_code}/recording/start` | Ad-hoc start; creates a `manual` session for the room. |
| `POST /api/v1/rooms/{room_code}/recording/stop` | Ad-hoc stop of the room's live session. |
| `POST /api/v1/rooms/{room_code}/camera/switch` | `{"camera_enum": N}` mid-session camera switch. |
| `POST /api/v1/rooms/{room_code}/photo` | Snapshot; answered with `{"session_photo_id", "request_id", "status": "pending"}`. |

Response statuses: session start/stop and the ad-hoc room stop answer `200` with the session; the
ad-hoc room start answers `201` with the new `manual` session and `Location: /api/v1/sessions/{id}`;
camera switch answers `202` with `{"command_id", "camera_enum"}`; the snapshot answers `202` with
the pending body above.

Snapshot ids are asynchronous: `POST …/photo` only starts the capture upstream, and webcam-server
returns the id later. dispatchub stores a `session_photos` row and polls
`GET /api/devices/{device_id}/photos`, matching on `request_id` until it can persist the real
`photo_id` (status `resolved`). If the TTL (`DISPATCH_PHOTO_POLL_TTL`, default `15m`) passes without
a match the row becomes `unresolved`; it is retried with a growing backoff, so an upstream outage
degrades into lag rather than a hot loop. A snapshot while the room is idle is legal and produces a
row with no session.

The current state of a room is proxied live:

```http
GET /api/v1/rooms/{room_code}/live
```

answers `{online, cameras: [...], active_session: {id, stream_id, started_at}}` from webcam-server.

## Stable error codes

Errors are `application/problem+json`; switch on the `detail` string:

| `detail` | Meaning |
| --- | --- |
| `invalid_request` | Malformed body, query or path. |
| `invalid_token` | The bearer token failed local verification. |
| `permission denied` | Verified caller, no candidate `dispatch:*` key allowed. |
| `term_not_found` | The referenced term does not exist. |
| `periods_not_configured` | The term has no 节次 table configured, or a request uses a period it does not cover. |
| `room_not_bound` | The room has no binding (or it is disabled). |
| `device_not_found` | webcam-server does not know the bound device. |
| `session_not_found` | The session does not exist. |
| `session_not_planned` | The manual start target is not in a state that can start. |
| `already_streaming` | A stream is already active on that device/camera (adopted by the scheduler, rejected for manual calls). |
| `device_offline` | webcam-server reports the device offline. |
| `no_active_stream` | The stop target has no live stream upstream. |
| `upstream_unavailable` | webcam-server could not be reached or answered 502. |
| `duplicate_idempotency_key` | The `X-Idempotency-Key` collides with a different command. |
| `duplicate_import` | The identical CSV was already imported for the term. |
| `session_collision` | An imported row overlaps an existing live or planned recording on the same device/camera. |
| `room_has_history` | The room is referenced by timetable or session history; disable it (`PUT` with `"enabled": false`) instead of deleting it. |
| `import_validation_failed` | One or more CSV rows failed validation; the full list is in the batch. |

`401` responses carry the teamusers decision body (`{"allow": false, "reason": "..."}`) instead of a
problem detail; an IAM outage answers `503` and never allows the request. See the
[API reference](/api/overview) for the per-route statuses.
