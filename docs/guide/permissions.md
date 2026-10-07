---
title: Permissions
outline: 2
---

# Permissions

Every `/api/v1` call is authenticated and authorized through
[teamusers](https://github.com/crazy4chicken/nsc-teamusers). dispatchub keeps no local accounts: it
verifies the bearer token locally against the issuer's JWKS and then resolves the caller's
permissions **remotely on every request** - permissions are never carried inside the token.

## The teamusers model

- teamusers is an Ed25519 (EdDSA) JWT issuer. Its JWKS lives at `{base}/.well-known/jwks.json`;
  access tokens are short-lived (TTL 600 s by default).
- Sign-in is `POST /auth/login {username,password}` and returns an access token and a refresh token.
  Services instead use `POST /auth/client-credentials {client_id,client_secret}`.
- A principal is a user `{id, username, display_name, ...}` plus an optional team. There is **no
  teacher/class/campus model** in teamusers: courses, rooms and teachers are dispatchub's own data.
- A permission is a three-segment key, matched literally segment by segment. The only wildcard is
  `*`, which matches exactly one segment.

## The nine keys

dispatchub's catalog consists of three independent permissions, each with three scopes. None
implies another:

| Key | Grants |
| --- | --- |
| `dispatch:read:<scope>` | Read terms, rooms, import history, sessions, artifacts and live room status. |
| `dispatch:manage:<scope>` | Create/update terms and 节次 tables; bind and unbind rooms; import timetables. |
| `dispatch:control:<scope>` | Start and stop recordings, switch cameras, take snapshots. |

In full:

```text
dispatch:read:any     dispatch:read:team     dispatch:read:own
dispatch:manage:any   dispatch:manage:team   dispatch:manage:own     # terms, rooms, imports
dispatch:control:any  dispatch:control:team  dispatch:control:own    # recording, camera switch, photo
```

A teacher who records their own lessons needs `dispatch:read` **and** `dispatch:control` at `own`
or `team` scope; an operator console needs `dispatch:read:any` plus `dispatch:control:any`; the
administrator who maintains rooms and imports needs `dispatch:manage` as well.

## The scope ladder

For every request dispatchub loads the target room (or the session's room) and builds the candidate
key list broadest-first:

```text
keys = ["dispatch:<action>:any"]
if room.team_id  != "" and room.team_id  == claims.team:    keys += ["dispatch:<action>:team"]
if room.owner_id != "" and room.owner_id == claims.subject: keys += ["dispatch:<action>:own"]
```

The first candidate the caller is allowed to use wins. None is implied by another:

- `any` needs no relationship to the room.
- `team` is only considered when the room's `team_id` equals the caller's `team`.
- `own` is only considered when the room's `owner_id` equals the caller's `sub`.

`team_id` and `owner_id` are **snapshotted onto the room row when the binding is created**
(`PUT /api/v1/rooms/{room_code}` reads them from the webcam-server device). That is why the binding
is the source of truth for scope resolution: if the device's ownership changes upstream, re-bind the
room to refresh the snapshot.

Collection and creation routes (`GET /api/v1/rooms`, `GET /api/v1/sessions`,
`GET/POST /api/v1/terms` and the import routes) have no single room to compare against, so no
`team`/`own` candidate is ever built for them: they require the `any`-scoped key
(`dispatch:read:any` or `dispatch:manage:any`).

One route is a hybrid: `PUT /api/v1/rooms/{room_code}` treats a missing room as a create, so a
**new** binding has no ownership snapshot and only `dispatch:manage:any` can grant it. Re-binding an
**existing** room keeps its snapshot and accepts `dispatch:manage:team` / `dispatch:manage:own`
through the same ladder.

## Endpoint matrix

`<scope>` resolves through the ladder above.

| Route | Permission |
| --- | --- |
| `GET /api/v1/terms`, `POST /api/v1/terms` | `dispatch:read:<scope>` / `dispatch:manage:<scope>` |
| `GET /api/v1/terms/{term_code}/periods`, `PUT .../periods` | `dispatch:read:<scope>` / `dispatch:manage:<scope>` |
| `GET /api/v1/rooms` | `dispatch:read:<scope>` |
| `PUT /api/v1/rooms/{room_code}`, `DELETE /api/v1/rooms/{room_code}` | `dispatch:manage:<scope>` |
| `POST /api/v1/timetable/imports` | `dispatch:manage:<scope>` |
| `GET /api/v1/timetable/imports`, `GET /api/v1/timetable/imports/{id}` | `dispatch:read:<scope>` |
| `GET /api/v1/sessions`, `GET /api/v1/sessions/{id}`, `GET /api/v1/sessions/{id}/artifacts` | `dispatch:read:<scope>` |
| `POST /api/v1/sessions/{id}/recording/start`, `POST /api/v1/sessions/{id}/recording/stop` | `dispatch:control:<scope>` |
| `POST /api/v1/rooms/{room_code}/recording/start`, `POST /api/v1/rooms/{room_code}/recording/stop` | `dispatch:control:<scope>` |
| `POST /api/v1/rooms/{room_code}/camera/switch` | `dispatch:control:<scope>` |
| `POST /api/v1/rooms/{room_code}/photo` | `dispatch:control:<scope>` |
| `GET /api/v1/rooms/{room_code}/live` | `dispatch:read:<scope>` |

`GET /healthz` and `GET /readyz` are public.

The generated [API reference](/api/overview) repeats the permission for each operation through its
`x-teamusers-permission` extension, which lists the `any` and `team` keys an endpoint accepts;
because that extension has only those two slots, the `own` scope is documented on this page only.

## Failure shapes

| Situation | Response |
| --- | --- |
| Missing, malformed or unverifiable token | `401` with the teamusers decision body `{"allow": false, "reason": "..."}` and `WWW-Authenticate: Bearer realm="teamusers"`. |
| Verified caller, no candidate key allowed | `403` RFC 9457 problem detail with `detail: "permission denied"`. |
| teamusers unreachable or erroring | `503` problem detail - dispatchub **fails closed**, never allows. |

## Catalog registration

A permission key must exist in teamusers' catalog before a role can carry it. Register all nine
once per deployment - either with the service subcommand:

```sh
DISPATCH_TEAMUSERS_URL='http://127.0.0.1:8082' \
DISPATCH_TEAMUSERS_ADMIN_TOKEN='<teamusers admin token>' \
  dispatchub register-permissions
```

which needs those two variables alone - no database, no webcam-server endpoint and no service
credential - and upserts each key with `POST /permissions/ {key, description, registered_by}`. The
same registration by hand:

```sh
TEAMUSERS_URL=http://127.0.0.1:8082
TEAMUSERS_ADMIN_TOKEN='<teamusers admin token>'

for key in \
  dispatch:read:any dispatch:read:team dispatch:read:own \
  dispatch:manage:any dispatch:manage:team dispatch:manage:own \
  dispatch:control:any dispatch:control:team dispatch:control:own
do
  curl -sS -X POST "$TEAMUSERS_URL/permissions/" \
    -H "Authorization: Bearer $TEAMUSERS_ADMIN_TOKEN" \
    -H 'Content-Type: application/json' \
    --data "{\"key\":\"$key\",\"description\":\"SmartClass dispatch hub\",\"registered_by\":\"smartclass-dispatchub\"}"
done
```

As a shorthand, one wildcard key per action (`dispatch:read:*`, `dispatch:manage:*`,
`dispatch:control:*`) can be registered instead and bound wherever all three scoped keys would have
been.

## Service credential

dispatchub is itself a teamusers client. Its credential -
`DISPATCH_TEAMUSERS_CLIENT_ID` / `DISPATCH_TEAMUSERS_CLIENT_SECRET` - authenticates the remote
permission lookups behind every inbound request, and the same credential is exchanged for the
outbound tokens used to call smartclass-webcam-server. The access token is cached and refreshed
before it expires; a `401` from webcam-server triggers exactly one forced refresh and retry.

For dispatchub to command webcam-server, the credential's principal must be bound to a role that
carries every `cam:*` key dispatchub uses. The union of the scope ladders it walks means the
practical requirement is:

- `cam:read:any` (device, stream, photo and segment reads)
- `cam:control:any` (recording start/stop, camera switch, snapshot)

Without them webcam-server answers `403` and the affected sessions fail with
`upstream_unavailable`/`permission denied` details.

## Teacher identity

`teacher_username` is stored verbatim from the CSV. Where a token `sub` is needed - `own` scope,
audit fields - dispatchub resolves it through a `teamusers_identities(username, subject_id,
synced_at)` cache, filled on first successful lookup. If teamusers has no username-to-subject lookup
endpoint available, `teacher_user_id` stays `NULL` and `own`-scoped access for that teacher degrades
to `team`/`any`. This is a known open item; verify the lookup endpoint in your phase 0 before
promising per-teacher self-service.
