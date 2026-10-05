# SmartClass Dispatch Hub

SmartClass Dispatch Hub turns a school's timetable into recorded lessons. You import the term's
schedule, bind each classroom to a camera, and the service does the rest: it starts each lesson's
recording on time, stops it at the end, keeps every recording and snapshot addressable afterwards,
and lets staff override the camera while a lesson is running.

It is the scheduling companion to
[SmartClass Webcam Server](https://github.com/crazy4chicken/smartclass-webcam-server): webcam-server
owns the devices and the media, dispatchub owns *when* and *why* they record.

## What it does

- **Timetable import** — upload a term's timetable as a CSV file (course, teacher, room, weekday,
  periods, weeks). Every row is validated before anything is stored, and a corrected file can be
  re-imported cleanly.
- **Automatic recording** — recordings start shortly before each lesson and stop at its end, with no
  operator involved. Missed or failed lessons are reported instead of disappearing.
- **Manual and mid-lesson control** — start or stop a recording early, switch the active camera, or
  take a snapshot at any moment, including outside the timetable.
- **Durable references** — each recording and each photo keeps the identifier issued by
  webcam-server, so a lesson's media stays findable long after it was captured. Recordings and
  snapshots are listed per session, with download links minted on demand.
- **Shared access control** — sign-in and permissions come from your existing `teamusers` IAM:
  read, manage and control are separate rights, scoped to your own rooms, your team, or the whole
  school.

## How it works

1. **Define the term.** Create the term with its first teaching week and its period table (the
   wall-clock time of each 节次).
2. **Bind the rooms.** Map each `room_code` from the timetable to a device and camera that
   webcam-server already knows, so the schedule knows which camera to drive.
3. **Import the timetable.** Upload the CSV. dispatchub validates it and expands it into concrete
   lesson sessions; from then on it starts and stops the recordings by itself.

While a lesson is live (or when nothing is scheduled), staff can take a picture or switch cameras
through the same API; every command is recorded for auditing.

## Requirements

- PostgreSQL 16 or newer — dispatchub keeps its own tables in the `smartclass_dispatchub` schema
  and applies its migrations on startup.
- A `smartclass-webcam-server` instance with its devices provisioned.
- A `teamusers` instance issuing your users' credentials and permission keys.
- A Linux host running [svchost](https://github.com/crazy4chicken/nekostick-svchost) (or any
  supervisor that can pass environment variables to a binary).

## Deployment

The full walkthrough — prerequisites, configuration reference, release artifact layout and a
complete `svchost.compose.yaml` for the fleet — is in
**[docs/guide/deploy.md](docs/guide/deploy.md)**.

The short version:

```sh
# 1. Build (or take the released binary from the GitHub release)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags '-s -w -X main.version=v0.1.0' -o dist/dispatchub ./cmd/dispatchub

# 2. Apply the database migrations
DISPATCH_DSN='postgres://dispatchub:…@db:5432/dispatchub?sslmode=require' \
  ./dispatchub migrate

# 3. Run it
DISPATCH_DSN='postgres://dispatchub:…@db:5432/dispatchub?sslmode=require' \
DISPATCH_TEAMUSERS_URL='https://iam.example.com' \
DISPATCH_TEAMUSERS_CLIENT_ID='dispatchub-svc' \
DISPATCH_TEAMUSERS_CLIENT_SECRET='…' \
DISPATCH_WEBCAM_URL='https://webcam.example.com' \
  ./dispatchub run
```

Releases are published automatically for `v*` tags as
`dispatchub_<version>_<arch>.zip` (Linux `x64` and `arm64`), which is exactly what svchost expects.

## Documentation

| Document | What it covers |
| --- | --- |
| [Deployment guide](docs/guide/deploy.md) | Installing, configuring and supervising dispatchub, including the fleet compose file. |
| [Getting started](docs/guide/getting-started.md) | A first run end to end: term, rooms, timetable, first recording. |
| [API usage](docs/guide/api-usage.md) | The workflow from import to artifacts, plus the mid-lesson control calls. |
| [Permissions](docs/guide/permissions.md) | The `teamusers` model, the permission keys and how to grant them. |
| [Operations](docs/guide/operations.md) | Scheduler behaviour, missed lessons, photo delays and upstream outages. |
| [API reference](docs/api/overview.md) | Every endpoint, generated from the running service. |

The same pages, rendered: <https://crazy4chicken.github.io/smartclass-dispatchub/>.

## License

Released under the GNU Affero General Public License v3.0, the same license as SmartClass Webcam
Server; see [LICENSE](LICENSE).
