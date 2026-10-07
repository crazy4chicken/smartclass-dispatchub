---
title: Getting Started
outline: 2
---

# Getting Started

This guide takes a source checkout to a locally running dispatchub that imports a timetable and
records one room. The minimum setup is one PostgreSQL database, a teamusers instance, and a
reachable smartclass-webcam-server with at least one connected device. For a first look you can
replace teamusers with `DISPATCH_DEV=true`.

## Prerequisites

- **Go 1.26 or newer** (build host only).
- **PostgreSQL 16 or newer.** dispatchub owns one database and applies its embedded goose
  migrations itself; no external migration tool is required. Everything it creates lives in the
  `smartclass_dispatchub` schema, never in `public`.
- **A teamusers instance.** Operators sign in there, and permission decisions are resolved against
  it on every request. The nine `dispatch:*` keys must be registered in its catalog; see
  [Permissions and access control](/guide/permissions).
- **A smartclass-webcam-server instance** with a registered, online device. dispatchub only *reads*
  devices and sends commands; device provisioning stays with webcam-server.

## Build

```sh
go build -trimpath -o dispatchub ./cmd/dispatchub
```

The subcommands are `run`, `migrate`, `status`, `doctor` and `register-permissions`.
`status` prints the redacted configuration, and `doctor` runs the same checks with a 30-second
timeout and exits non-zero when a check fails.

Each subcommand requires only what it uses: `migrate` needs `DISPATCH_DSN`,
`register-permissions` needs `DISPATCH_TEAMUSERS_URL` plus an admin token (`-token` or
`DISPATCH_TEAMUSERS_ADMIN_TOKEN`), and `status` and `doctor` run against whatever is configured -
they print `SKIP` for a dependency that is not.

## Configure

The service reads everything from the environment (a `.env` file is loaded only when
`DISPATCH_ENV_FILE` points at one). A minimal local setup:

```sh
export DISPATCH_DSN='postgres://dispatchub:dispatchub@127.0.0.1:5432/dispatchub?sslmode=disable'
export DISPATCH_TEAMUSERS_URL='http://127.0.0.1:8082'
export DISPATCH_TEAMUSERS_CLIENT_ID='dispatchub-svc'
export DISPATCH_TEAMUSERS_CLIENT_SECRET='...'
export DISPATCH_WEBCAM_URL='http://127.0.0.1:8080'
```

Every variable, its default and its meaning is listed in the
[deployment guide](/guide/deploy#configuration). Two values are easy to get wrong on day one:

- `DISPATCH_TEAMUSERS_AUD` must equal the audience the issuer mints (the fleet convention is
  `nekostick`, not `teamusers`).
- `DISPATCH_TIMEZONE` (default `Asia/Shanghai`) is what turns `(date, period)` rows into instants;
  a wrong value silently schedules the whole term at the wrong time.

For a throwaway run without teamusers, set `DISPATCH_DEV=true`: the service injects synthetic claims
and skips the issuer entirely. Never enable it on a reachable host.

## Start

```sh
createdb dispatchub
./dispatchub run
```

`run` applies the embedded migrations (`./dispatchub migrate` applies them without starting the
server) and listens on `DISPATCH_LISTEN_ADDR`, `:8081` by default.

```sh
curl -sS http://127.0.0.1:8081/healthz   # liveness, always 200 while the process lives
curl -sS http://127.0.0.1:8081/readyz    # Postgres ping + webcam-server /readyz
```

## Get an access token

With a real teamusers instance:

```sh
export BASE_URL='http://127.0.0.1:8081'
export TOKEN="$(curl -sS -X POST "$DISPATCH_TEAMUSERS_URL/auth/login" \
  -H 'Content-Type: application/json' \
  -d '{"username":"operator","password":"..."}' | jq -r .access_token)"
```

With `DISPATCH_DEV=true` no `Authorization` header is needed, so the `curl` calls below work
without `-H "Authorization: Bearer $TOKEN"`.

## 1. Create a term

A term is a `term_code`, a display name, the Monday of teaching week 1 and the number of teaching
weeks (1-30):

```sh
curl -sS -X POST "$BASE_URL/api/v1/terms" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"term_code":"2026-FALL","name":"2026 秋季学期","week1_monday":"2026-09-07","weeks":16}'
```

## 2. Define the 节次 time table

Periods belong to a term and replace the whole set on every `PUT`. Each entry maps a period number
(1-20) to its wall-clock start and end, in `DISPATCH_TIMEZONE`:

```sh
curl -sS -X PUT "$BASE_URL/api/v1/terms/2026-FALL/periods" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"periods":[
    {"period_no":1,"start_time":"08:00","end_time":"08:45"},
    {"period_no":2,"start_time":"08:55","end_time":"09:40"},
    {"period_no":3,"start_time":"10:00","end_time":"10:45"},
    {"period_no":4,"start_time":"10:55","end_time":"11:40"}
  ]}'
```

An import that touches a period the term has not configured is rejected with
`periods_not_configured`. `PUT` answers `200` with the replaced set in the usual
`{"items": [...]}` envelope, and a `GET` on the same route returns the same list (or
`periods_not_configured` when the term has none).

## 3. Bind a room

A binding maps the timetable's `room_code` to one camera on one webcam-server device. dispatchub
verifies the device through `GET /api/devices/{device_id}/` and snapshots its `team_id`/`owner_id`
for scoped permissions:

```sh
curl -sS -X PUT "$BASE_URL/api/v1/rooms/A301" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"device_id":"01J8Z4W3K5M7Q9R1T3V5X7Z9B1","camera_enum":0,"name":"A301 教室"}'
```

`camera_enum` is the camera number the device reports (`GET /api/devices/{device_id}/` lists
them). Binding the same `(device_id, camera_enum)` twice is rejected. The body also accepts an
optional `"enabled": false` to keep the binding but make imports treat the room as unbound
(`room_not_bound`) until it is re-enabled.

A binding can only be deleted while nothing references it: once the room appears in timetable
entries or session history, `DELETE /api/v1/rooms/{room_code}` answers `409 room_has_history` and
the room should be disabled instead (`PUT` with `"enabled": false`), which takes it out of
scheduling while keeping the historical rows readable. That flip works at any time, history or
not.

## 4. Import the timetable

The CSV has one row per (course, room, weekday, period range, weeks) and this required header,
matched by name in any order:

```csv
term_code,course_code,course_name,teacher_username,room_code,weekday,period_start,period_end,weeks
2026-FALL,CS101,数据结构,zhangsan,A301,1,1,2,"1-16"
2026-FALL,CS101,数据结构,zhangsan,A301,3,3,4,"1-16"
2026-FALL,MA201,高等数学,lisi,B102,5,5,6,"1-8,10-16"
```

Always dry-run first: validation reports every row error, writes nothing but the batch row, and
answers `200` with the import plus a preview of the first/last occurrence per row.

```sh
curl -sS -X POST "$BASE_URL/api/v1/timetable/imports?dry_run=true&mode=replace" \
  -H "Authorization: Bearer $TOKEN" \
  -F 'file=@timetable.csv'
```

Then repeat the call without `dry_run=true` to commit (answers `201` with the import). A commit is
one transaction: batch, entries and the expanded `sessions` all land together, and any blocking
row error answers `422` with the full error list and no partial commit. `mode=append` keeps the
term's existing entries; the default `replace` supersedes them and cancels their future `planned`
sessions. Re-uploading an identical file answers `409 duplicate_import` unless `?force=true`.

Verify what was materialized:

```sh
curl -sS "$BASE_URL/api/v1/sessions?term_code=2026-FALL" \
  -H "Authorization: Bearer $TOKEN"
```

## 5. Record a room

Ad-hoc control works without any schedule:

```sh
curl -sS -X POST "$BASE_URL/api/v1/rooms/A301/recording/start" \
  -H "Authorization: Bearer $TOKEN"

curl -sS -X POST "$BASE_URL/api/v1/rooms/A301/recording/stop" \
  -H "Authorization: Bearer $TOKEN"
```

The start call asks webcam-server to start `camera_enum` on the bound device and stores the
returned `stream_id` on a new `manual` session. `GET /api/v1/sessions/{id}/artifacts` then lists the
recording's segments and photos with freshly minted download URLs. Scheduled sessions need no
call at all: the scheduler starts them `DISPATCH_PRESTART` before `starts_at` and stops them at
`ends_at`.

## Next steps

- [Deployment](/guide/deploy) - the fleet compose file and the full configuration reference.
- [Permissions](/guide/permissions) - registering the `dispatch:*` keys and the scope ladder.
- [API Usage](/guide/api-usage) - the full import-to-artifacts walkthrough and mid-session calls.
- [Operations](/guide/operations) - scheduler semantics, `missed`/`failed` triage, outages.
