---
title: Deployment
outline: 2
---

# Deployment

This guide covers the Nekostick/svchost path used by the SmartClass fleet: one compose document
declares the four services (teamusers, filewarehouse, webcam-server, dispatchub), the Host injects
addresses, ports and secrets, and dispatchub is deployed as a singleton scheduler.

## Prerequisites

- **PostgreSQL 16 or newer**, reachable from the dispatchub launch. The service applies its
  embedded goose migrations on startup; no external migration tool is needed. Every object it
  creates lives in the schema `smartclass_dispatchub`, which it creates on the first migrate, so
  the DSN role needs no rights on the database's `public` schema. A role that does not own the
  database cannot create a schema: a DBA creates it once and grants it, and dispatchub then uses
  it as-is.
- **A teamusers instance** with the nine `dispatch:*` keys registered and a service credential for
  dispatchub. See [Permissions and access control](/guide/permissions).
- **A smartclass-webcam-server instance** with its devices provisioned and an nsc-filehouse (or
  S3) media sink configured upstream. dispatchub stores only ids; it never touches the media.
- **svchost** or any other supervisor that can run a Linux binary with environment variables.

## Release artifact

The release workflow publishes `dispatchub_<version>_<arch>.zip` for `linux/amd64` (`x64`) and
`linux/arm64`, with the `dispatchub` binary at the ZIP root and nothing else. That layout is the
svchost contract: `services.<name>` in the compose document is the service id, and the entry binary
must carry the exact service name.

To build the same artifact locally:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags '-s -w -X main.version=v0.1.0' -o dist/dispatchub ./cmd/dispatchub
```

## Configuration

Every value comes from the environment. A `.env` file is loaded only when `DISPATCH_ENV_FILE` is
set, and real environment variables always win over it:

> precedence: environment > `DISPATCH_ENV_FILE` file > default

### Configuration reference

| Variable | Default | Required | Purpose |
| --- | --- | --- | --- |
| `DISPATCH_LISTEN_ADDR` | `:8081` | No | HTTP listen address. |
| `DISPATCH_DSN` | - | Yes | PostgreSQL DSN; the pool sets `application_name=smartclass-dispatchub` and pins `search_path` to the `smartclass_dispatchub` schema (a DSN that sets `search_path` itself wins). |
| `DISPATCH_TIMEZONE` | `Asia/Shanghai` | No | Timezone used to turn `(date, period)` into instants. |
| `DISPATCH_TEAMUSERS_URL` | - | Yes (unless `DISPATCH_DEV=true`) | teamusers base URL; JWKS is discovered under it. |
| `DISPATCH_TEAMUSERS_ISSUER` | `teamusers` | No | Expected `iss` claim. |
| `DISPATCH_TEAMUSERS_AUD` | `teamusers` | No | Expected `aud` claim - set it to the fleet value (`nekostick`). |
| `DISPATCH_TEAMUSERS_CLIENT_ID` | - | Yes (unless `DISPATCH_DEV=true`) | client-credentials credential id for outbound calls (webcam-server + permission lookups). |
| `DISPATCH_TEAMUSERS_CLIENT_SECRET` | - | Yes (unless `DISPATCH_DEV=true`) | client-credentials secret. Treat as a secret. |
| `DISPATCH_TEAMUSERS_TIMEOUT` | `5s` | No | IAM HTTP timeout. |
| `DISPATCH_WEBCAM_URL` | - | Yes (unless `DISPATCH_DEV=true`) | smartclass-webcam-server base URL, e.g. `http://webcam:8080`. |
| `DISPATCH_WEBCAM_TIMEOUT` | `10s` | No | Outbound HTTP timeout. |
| `DISPATCH_SCHED_TICK` | `30s` | No | Scheduler scan interval. |
| `DISPATCH_PRESTART` | `2m` | No | How early a session's recording starts. |
| `DISPATCH_POSTSTOP_GRACE` | `0s` | No | Delay before stopping at `ends_at`. |
| `DISPATCH_START_RETRY_MAX` | `3` | No | Start attempts before a session is marked `failed`. |
| `DISPATCH_MISS_GRACE` | `10m` | No | Past `starts_at + grace` a `planned` session becomes `missed`. |
| `DISPATCH_PHOTO_POLL_INTERVAL` | `2s` | No | Photo-id resolution poll interval. |
| `DISPATCH_PHOTO_POLL_TTL` | `15m` | No | Give up on a photo (status `unresolved`) after this. |
| `DISPATCH_MAX_CONCURRENT_COMMANDS` | `8` | No | Outbound command concurrency cap. |
| `DISPATCH_MAX_IMPORT_ROWS` | `5000` | No | CSV row cap per import. |
| `DISPATCH_DEV` | `false` | No | Dev bypass: synthetic claims, teamusers disabled. Local development only. |
| `DISPATCH_LOG_LEVEL` | `info` | No | `slog` level: `debug`, `info`, `warn` or `error`. |
| `DISPATCH_ENV_FILE` | - | No | Path to a `.env`-style file; loaded at startup only when set. |

`DISPATCH_*_TIMEOUT`, `DISPATCH_SCHED_TICK`, `DISPATCH_PRESTART`, `DISPATCH_POSTSTOP_GRACE`,
`DISPATCH_MISS_GRACE`, `DISPATCH_PHOTO_POLL_INTERVAL` and `DISPATCH_PHOTO_POLL_TTL` accept Go
duration syntax (`300ms`, `2m`, `1h`). `Validate()` fails fast on a missing required value, an
unparsable duration, an unknown timezone or an unknown log level, so a bad compose edit surfaces at
startup, not at the first lesson.

## Fleet deployment (svchost)

The document below is the complete `svchost.compose.yaml` for the fleet; the same file is committed
at the repository root. Field semantics are defined in the
[svchost compose reference](https://github.com/crazy4chicken/nekostick-svchost/blob/main/docs/compose.md).

```yaml
# svchost compose for the SmartClass dispatch fleet.
# Field semantics: https://github.com/crazy4chicken/nekostick-svchost/blob/main/docs/compose.md
#
# strictSources is intentionally omitted (it defaults to false): a single
# source.sha256 cannot cover every architecture at once, because the per-arch
# ZIP assets have different digests. Pin sha256 only for a single-architecture
# deployment, or set strictSources: true together with a matching digest.
serviceScope: global

services:
  teamusers:
    source:
      release: "github:crazy4chicken/nsc-teamusers@v0.2.1" # asset teamusers_<version>_<arch>.zip, entry binary `teamusers`
      # sha256: "..." # optional single-arch pin; otherwise svchost verifies against the GitHub asset digest
    args: ["run"]
    env:
      TEAMUSERS_CONNECTION_STRING: "${HOST:TEAMUSERS_CONNECTION_STRING}"
      TEAMUSERS_KEY_DIR: /var/lib/teamusers/keys # persistent and writable; never point it at artifacts/ or tmp/
      TEAMUSERS_NODE_ID: teamusers
      TEAMUSERS_TOKEN_AUDIENCE: nekostick # MUST equal DISPATCH_TEAMUSERS_AUD and WEBCAM_TEAMUSERS_AUD
      TEAMUSERS_LOG_LEVEL: info
      # The listen address comes from the Host launch values; override with
      # TEAMUSERS_LISTEN_ADDRESS / TEAMUSERS_LISTEN_PORT if this deployment needs fixed values.
    start: eager # authentication is on the fleet's critical path, start with the host
    restart: on-failure
    health:
      type: http
      path: /healthz
      timeout: 5s
    route:
      prefix: /iam
      strip: true # /iam/auth/login reaches the child process as /auth/login

  filewarehouse:
    source:
      release: "github:crazy4chicken/nsc-filewarehouse@v0.1.0" # module nsc-filehouse; entry binary `filewarehouse`
      # sha256: "..." # optional single-arch pin
    args: ["run"]
    env:
      FILEHOUSE_DSN: "${HOST:FILEHOUSE_DSN}"
      FILEHOUSE_ADDR: "${HOST}"
      FILEHOUSE_PORT: "${PORT}"
      FILEHOUSE_BLOB_DIR: /var/lib/filehouse/blobs # persistent and writable; the segments and photos live here
      FILEHOUSE_KEY_DIR: /var/lib/filehouse/keys
      FILEHOUSE_NODE_ID: filewarehouse
      FILEHOUSE_PUBLIC_BASE_URL: "https://files.example.com/files" # public origin, route prefix included
      FILEHOUSE_TEAMUSERS_BASE_URL: "http://${HOST@teamusers}:${PORT@teamusers}"
      FILEHOUSE_TEAMUSERS_ISSUER: teamusers
      FILEHOUSE_TEAMUSERS_AUDIENCE: nekostick
      FILEHOUSE_TEAMUSERS_CLIENT_ID: filehouse-svc
      FILEHOUSE_TEAMUSERS_CLIENT_SECRET: "${HOST:FILEHOUSE_TEAMUSERS_CLIENT_SECRET}"
    start: eager # applies migrations at startup, before the first upload
    restart: on-failure
    health:
      type: http
      path: /healthz
      timeout: 5s
    route:
      prefix: /files
      strip: true

  webcam-server:
    source:
      release: "github:crazy4chicken/smartclass-webcam-server@v0.2.0"
    env:
      WEBCAM_LISTEN_ADDR: "${HOST}:${PORT}" # the Host assigns the port for this launch
      WEBCAM_DB_URL: "${HOST:WEBCAM_DB_URL}"
      WEBCAM_TEAMUSERS_URL: "http://${HOST@teamusers}:${PORT@teamusers}"
      WEBCAM_TEAMUSERS_AUD: nekostick
      WEBCAM_TEAMUSERS_CLIENT_ID: webcam-server-svc
      WEBCAM_TEAMUSERS_CLIENT_SECRET: "${HOST:WEBCAM_TEAMUSERS_CLIENT_SECRET}"
      WEBCAM_FILEHOUSE_URL: "http://${HOST@filewarehouse}:${PORT@filewarehouse}" # internal address, no route prefix; the media sink — omit it and webcam-server falls back to S3, then to noop (nothing is stored)
      WEBCAM_FILEHOUSE_BUCKET: webcam-segments
      WEBCAM_WS_TICKET_TTL: 60s
    start: eager
    restart: on-failure
    health:
      type: http
      path: /readyz # also covers the Postgres connection
      timeout: 5s
    route:
      prefix: /webcam
      strip: true # see the device-plane note below before publishing this prefix

  dispatchub:
    source:
      release: "github:crazy4chicken/smartclass-dispatchub@v0.1.0"
      # Before the first release exists, bootstrap from a local binary instead:
      # path: /opt/dispatchub/dispatchub
    env:
      DISPATCH_LISTEN_ADDR: "${HOST}:${PORT}"
      DISPATCH_DSN: "${HOST:DISPATCH_DSN}"
      DISPATCH_TIMEZONE: Asia/Shanghai
      DISPATCH_TEAMUSERS_URL: "http://${HOST@teamusers}:${PORT@teamusers}"
      DISPATCH_TEAMUSERS_ISSUER: teamusers
      DISPATCH_TEAMUSERS_AUD: nekostick
      DISPATCH_TEAMUSERS_CLIENT_ID: dispatchub-svc
      DISPATCH_TEAMUSERS_CLIENT_SECRET: "${HOST:DISPATCH_TEAMUSERS_CLIENT_SECRET}"
      DISPATCH_WEBCAM_URL: "http://${HOST@webcam-server}:${PORT@webcam-server}" # same-namespace service reference
      DISPATCH_PRESTART: 2m
      DISPATCH_POSTSTOP_GRACE: 15s
      DISPATCH_LOG_LEVEL: info
    start: eager # the scheduler must be up before the first lesson window
    restart: on-failure # fatal errors exit non-zero; use `always` if a clean exit should also be restarted
    health:
      type: http
      path: /healthz
      timeout: 5s
    route:
      prefix: /dispatch
      strip: true # /dispatch/api/v1/... reaches the child process as /api/v1/...
```

Before applying it to a real host, replace the two example values: the filehouse public origin
(`https://files.example.com/files`) and the release versions if you pin a different one.

### Template walkthrough

svchost resolves three template forms inside `env` values:

| Template | Meaning |
| --- | --- |
| `${HOST:NAME}` | A **host value** stored on the svchost Host: every secret and every DSN is passed through this form. Nothing is stored in the compose file. |
| `${HOST}` / `${PORT}` | The address and port the Host leased to **this** launch. `WEBCAM_LISTEN_ADDR` and `DISPATCH_LISTEN_ADDR` bind to them. |
| `${HOST@service}` / `${PORT@service}` | The address and port assigned to **another service in the same namespace**, e.g. `http://${HOST@teamusers}:${PORT@teamusers}`. This is why no static port appears anywhere in the fleet. |

### Notes

- **No secret ever lives in the YAML.** Every credential is a `${HOST:...}` passthrough and every
  DSN comes from the host environment, so the file is committed as-is and the values stay in the
  Host.
- **Cross-service wiring uses launch templates.** `${HOST@teamusers}:${PORT@teamusers}` reads the
  referenced service's Host-assigned address, so no static port appears anywhere in the fleet; the
  target must be declared in the same global namespace (it is, above).
- **The service name is the release contract.** `services.<name>` is the `serviceId`, so the release
  workflow must publish `dispatchub_<version>_<arch>.zip` with the entry binary named exactly
  `dispatchub` at the ZIP root, and the process CWD will be the service root rather than that
  extraction directory.
- **A dangling `${...@svc}` reference fails that service's sync.** Dropping the `filewarehouse` block
  means also dropping `WEBCAM_FILEHOUSE_URL`; an unresolvable service-name target is a sync failure,
  not a silent empty string. (The `nsc-filewarehouse` compose circulating in the fleet sets
  `FILEWAREHOUSE_*` names that do not match the binary's `FILEHOUSE_*` config constants - use the
  names above.)
- **Health paths differ in meaning.** `/healthz` is the liveness check the supervisor should use;
  `/readyz` additionally probes Postgres and webcam-server, so it belongs on the operator dashboard.
- **Device-plane exposure is an explicit decision.** `route.prefix: /webcam` publishes the entire
  webcam-server surface, including the `wdt_`-token device plane (`/ws/register`,
  `/ws/device/{ticket}`). If devices must not be reachable through the host router, give
  webcam-server its own Host/compose without a `route` block and keep only dispatchub behind the
  gateway - dispatchub reaches it over the internal `${HOST@webcam-server}` address either way.
- **One digest, one architecture.** `source.sha256` holds a single value, so it cannot match both the
  x64 and arm64 ZIPs; pin it for single-architecture deployments, or set `strictSources: true`
  together with the matching digest.
- **Scope choice.** `serviceScope: global` (used above) shares one service root and one service-name
  namespace across configs - a duplicate service name makes the whole reconciliation round fail -
  while `serviceScope: document` isolates roots per config name. dispatchub must be declared exactly
  once in the global namespace, and it is a singleton scheduler: extra replicas idle on the advisory
  lock, they do not split work.

### Field notes

`teamusers`

| Field | Note |
| --- | --- |
| `source.release` | `v0.2.1` publishes `teamusers_<version>_<arch>.zip` with the `teamusers` binary at the ZIP root. |
| `source.sha256` | Optional; pin only for one architecture (see the digest note above). |
| `args` | `["run"]` - the same subcommand the release artifact expects. |
| `TEAMUSERS_CONNECTION_STRING` | The teamusers database DSN, from the Host. |
| `TEAMUSERS_KEY_DIR` | Persistent and writable; holds the Ed25519 signing keys. Never point it at `artifacts/` or `tmp/`. |
| `TEAMUSERS_TOKEN_AUDIENCE` | Must equal `DISPATCH_TEAMUSERS_AUD` and `WEBCAM_TEAMUSERS_AUD`. |
| `start` / `restart` | `eager` / `on-failure`: authentication is on the fleet's critical path. |
| `health` | `GET /healthz` is the supervisor's liveness probe. |
| `route` | `prefix: /iam`, `strip: true`: `/iam/auth/login` reaches the child as `/auth/login`. |

`filewarehouse`

| Field | Note |
| --- | --- |
| `source.release` | The module is `nsc-filehouse`; the asset ships the `filewarehouse` binary. |
| `args` | `["run"]` - applies migrations at startup, before the first upload. |
| `FILEHOUSE_DSN` | The filehouse database DSN, from the Host. |
| `FILEHOUSE_ADDR` / `FILEHOUSE_PORT` | Bound to the launch's `${HOST}`/`${PORT}` lease. |
| `FILEHOUSE_BLOB_DIR` | Persistent and writable; segments and photos live here. |
| `FILEHOUSE_KEY_DIR` | Persistent; filehouse signing/encryption keys. |
| `FILEHOUSE_PUBLIC_BASE_URL` | The public origin including the route prefix; replace the example host. |
| `FILEHOUSE_TEAMUSERS_*` | Inbound token verification and the service credential; audience `nekostick`. |
| `health` | `GET /healthz`. |
| `route` | `prefix: /files`, `strip: true`. |

`webcam-server`

| Field | Note |
| --- | --- |
| `source.release` | Pushing a `v*` tag publishes `smartclass-webcam-server_<version>_<arch>.zip`. |
| `WEBCAM_LISTEN_ADDR` | `${HOST}:${PORT}` - the Host assigns the port for this launch. |
| `WEBCAM_DB_URL` | webcam-server's own database DSN, from the Host. |
| `WEBCAM_TEAMUSERS_URL` | Internal address of the teamusers launch; no route prefix. |
| `WEBCAM_TEAMUSERS_AUD` | Must equal the issuer's `TEAMUSERS_TOKEN_AUDIENCE`. |
| `WEBCAM_TEAMUSERS_CLIENT_ID` / `..._SECRET` | webcam-server's service credential (also used for filehouse). |
| `WEBCAM_FILEHOUSE_URL` | The media sink over the internal address. Omit it and webcam-server falls back to S3, then to a no-op backend that stores nothing. |
| `WEBCAM_FILEHOUSE_BUCKET` | Bucket for segments; it must exist. |
| `WEBCAM_WS_TICKET_TTL` | Lifetime of the device registration ticket. |
| `health` | `GET /readyz` also covers the Postgres connection. |
| `route` | `prefix: /webcam`, `strip: true`; read the device-plane note above before publishing it. |

`dispatchub`

| Field | Note |
| --- | --- |
| `source.release` | `dispatchub_<version>_<arch>.zip`, binary `dispatchub`; the commented `path:` is the bootstrap route before the first release exists. |
| `DISPATCH_LISTEN_ADDR` | `${HOST}:${PORT}` - bind to the launch lease. |
| `DISPATCH_DSN` | The dispatchub database DSN; migrations are applied from the embedded FS at startup. |
| `DISPATCH_TIMEZONE` | Keep `Asia/Shanghai` unless the school's wall clock says otherwise; a wrong value schedules the whole term wrongly. |
| `DISPATCH_TEAMUSERS_URL` | Internal teamusers address; JWKS is discovered under it. |
| `DISPATCH_TEAMUSERS_ISSUER` / `..._AUD` | `teamusers` / `nekostick`; the audience must match `TEAMUSERS_TOKEN_AUDIENCE`. |
| `DISPATCH_TEAMUSERS_CLIENT_ID` / `..._SECRET` | dispatchub's service credential; the secret comes from the Host. |
| `DISPATCH_WEBCAM_URL` | Same-namespace service reference to webcam-server. |
| `DISPATCH_PRESTART` / `DISPATCH_POSTSTOP_GRACE` | Example overrides (`2m` / `15s`); both default differently, see the reference table. |
| `DISPATCH_LOG_LEVEL` | `slog` JSON level. |
| `start` / `restart` | `eager` (the scheduler must be up before the first lesson window) / `on-failure` (fatal errors exit non-zero; use `always` to also restart clean exits). |
| `health` | `GET /healthz` for the supervisor. Use `/readyz` on the dashboard: it also probes Postgres and webcam-server. |
| `route` | `prefix: /dispatch`, `strip: true`: `/dispatch/api/v1/...` reaches the child as `/api/v1/...`. |

## First-time setup

1. **Store the host values** referenced by the compose file:
   `TEAMUSERS_CONNECTION_STRING`, `FILEHOUSE_DSN`, `WEBCAM_DB_URL`, `DISPATCH_DSN`,
   `TEAMUSERS_CLIENT_SECRET` equivalents used above (`FILEHOUSE_TEAMUSERS_CLIENT_SECRET`,
   `WEBCAM_TEAMUSERS_CLIENT_SECRET`, `DISPATCH_TEAMUSERS_CLIENT_SECRET`).
2. **Register the nine `dispatch:*` keys** in teamusers and bind them to roles. Either run
   `dispatchub register-permissions` against the instance or use the raw registration calls in the
   [permissions guide](/guide/permissions#catalog-registration).
3. **Create the service credentials** (`filehouse-svc`, `webcam-server-svc`, `dispatchub-svc`) in
   teamusers and place their secrets in the Host values.
4. **Give the dispatchub credential the `cam:*` keys it needs** to call webcam-server: in practice
   `cam:read:any` and `cam:control:any` cover the union of the scope ladders dispatchub uses.
5. **Bind at least one room** (`PUT /api/v1/rooms/{room_code}`) before the first import, otherwise
   every CSV row fails validation with `room_not_bound`.
6. **Apply the compose document** and verify: `GET /dispatch/readyz` answers 200 through the host
   route, and `dispatchub status` prints the redacted configuration with a reachable database and
   webcam-server.

## Health and diagnostics

| Probe | Meaning |
| --- | --- |
| `GET /healthz` | Liveness: 200 while the process lives. This is the supervisor's probe. |
| `GET /readyz` | Readiness: Postgres ping plus `GET {DISPATCH_WEBCAM_URL}/readyz`. Use it on the operator dashboard. |
| `dispatchub status` | Prints the redacted configuration, the database version and webcam-server reachability, then exits. |
| `dispatchub doctor` | Runs the same checks with a 30 s timeout and exits non-zero on failure. Suitable for a cron or a deployment gate. |

Security notes:

- `DISPATCH_DSN` and `DISPATCH_TEAMUSERS_CLIENT_SECRET` are secrets. Never commit them, paste them
  into an issue, or print them; `status` replaces secret values with `<redacted>`.
- `DISPATCH_DEV=true` disables teamusers entirely. Never set it on a host that is reachable from a
  network you do not control.
