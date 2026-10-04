---
title: API Overview
---

# API Overview

smartclass-dispatchub exposes one HTTP API for operators and management tools: it keeps the
classroom recording schedule, imports timetables, and drives smartclass-webcam-server. The
generated reference is produced from the same route table the server serves, so it can never drift
from the running code.

## Base URL

All paths in this reference are relative to the server's listen address, `DISPATCH_LISTEN_ADDR`
(default `:8081`), and live under `/api/v1`:

```text
http://dispatch.example.com:8081/api/v1/terms
```

In the fleet, the svchost `route.prefix: /dispatch` is stripped before the request reaches the
process, so the public URL is `https://<host>/dispatch/api/v1/terms`. See the
[deployment guide](/guide/deploy) for how the service is exposed.

## Authentication

Every `/api/v1` call needs a teamusers-issued bearer token:

```http
Authorization: Bearer <access token>
```

The token is verified locally against the issuer's JWKS, then the caller's `dispatch:*`
permissions are resolved remotely - permissions are never carried inside the token. The key
grammar is `dispatch:<action>:<scope>` with `action ∈ {read, manage, control}` and
`scope ∈ {own, team, any}`, resolved through the `any > team > own` ladder:

- a request without valid claims answers `401` (teamusers decision body plus
  `WWW-Authenticate: Bearer realm="teamusers"`);
- a verified token that does not reach the target answers `403` with the problem detail
  `permission denied`;
- when the authorization service is unreachable the request answers `503` - dispatchub fails
  closed, never open.

The ladder, the nine keys and the service credential are documented in
[Permissions and access control](/guide/permissions).

## Errors

Failures that reach a handler are reported as RFC 9457 problem details with the content type
`application/problem+json`:

```json
{
  "type": "about:blank",
  "title": "Not Found",
  "status": 404,
  "detail": "session_not_found",
  "instance": "/api/v1/sessions/01J8Z4W3K5M7Q9R1T3V5X7Z9B1"
}
```

| Field | JSON type | Present | Meaning |
| --- | --- | --- | --- |
| `type` | string | always | Always `about:blank`; the service defines no error catalogue of its own. |
| `title` | string | always | The HTTP status text of `status`. |
| `status` | integer | always | The HTTP status code, repeated in the body. |
| `detail` | string | always | A **stable code** from the table below - switch on it, not on `title`. |
| `instance` | string | always | Path of the failing request. |

| `detail` | Returned when |
| --- | --- |
| `invalid_request` | Malformed body, query or path. |
| `invalid_token` | The bearer token failed verification. |
| `permission denied` | Verified caller, no candidate `dispatch:*` key allowed. |
| `term_not_found` | Unknown `term_code`. |
| `periods_not_configured` | The term has no 节次 table configured, or a request uses a period it does not cover. |
| `room_not_bound` | The room has no (enabled) binding. |
| `device_not_found` | The bound device does not exist in webcam-server. |
| `session_not_found` | Unknown session id. |
| `session_not_planned` | The session is not in a state that supports the requested start. |
| `already_streaming` | A stream is already active on that device/camera. |
| `device_offline` | webcam-server reports the device offline. |
| `no_active_stream` | The stop target has no live stream upstream. |
| `upstream_unavailable` | webcam-server unreachable or answered 502. |
| `duplicate_idempotency_key` | `X-Idempotency-Key` collides with a different command. |
| `duplicate_import` | The identical CSV was already imported for the term. |
| `import_validation_failed` | One or more CSV rows failed validation; the batch carries the full list. |

`401` responses carry the teamusers decision body (`{"allow": false, "reason": "..."}`) instead of
problem details; a `403` from the scope ladder is a problem detail with `permission denied`.

## Collections

List endpoints wrap their results in an `items` envelope, so a response is always a JSON object and
never a bare array:

```json
{
  "items": [
    { "term_code": "2026-FALL", "name": "2026 秋季学期", "weeks": 16 }
  ]
}
```

An empty collection returns `{"items": []}`. List endpoints accept filters; sessions for example:

```http
GET /api/v1/sessions?term_code=2026-FALL&room_code=A301&status=planned
```

supported keys are `term_code`, `room_code`, `date` (`YYYY-MM-DD`), `status`, `course_code` and
`limit`, and results come back newest `starts_at` first. `date` matches a UTC calendar day against
`starts_at` (`[00:00, next 00:00) UTC`), independent of `DISPATCH_TIMEZONE`.

All list routes apply the same `limit` rules:

- the default is `100` when `limit` is omitted;
- the maximum is `1000`; larger values are clamped, not rejected;
- a non-numeric or non-positive value is rejected with `400` (`invalid_request`).

Control POSTs accept an optional `X-Idempotency-Key`; replaying a request with the same key returns
the stored outcome without issuing a second upstream command.

## Reference

One page per resource domain, generated from the OpenAPI document:

- [Terms](/api/reference/terms) - terms and their 节次 time tables.
- [Rooms](/api/reference/rooms) - room-to-device bindings and live room status.
- [Timetable](/api/reference/timetable) - CSV import history and per-row reports.
- [Sessions](/api/reference/sessions) - materialized recording sessions and their artifacts.
- [Recording](/api/reference/recording) - manual start/stop, camera switch and snapshots.
- [Health](/api/reference/health) - liveness and readiness probes.

The machine-readable document these pages are built from is available for download:
[openapi.yaml](/openapi.yaml).
