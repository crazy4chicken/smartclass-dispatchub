---
layout: home

hero:
  name: SmartClass Dispatch Hub
  text: Timetable-driven classroom recording
  tagline: Imports course timetables, binds rooms to smartclass-webcam-server devices, starts and stops each lesson's recording automatically, and keeps every stream id and photo id addressable in PostgreSQL.
  actions:
    - theme: brand
      text: Get Started
      link: /guide/getting-started
    - theme: alt
      text: Deploy
      link: /guide/deploy

features:
  - title: Timetable import
    details: Upload a CSV of course, teacher, room and 节次 rows. dispatchub validates it, expands the week ranges and materializes one recording session per occurrence - dry-run first, never a partial commit.
  - title: Room binding
    details: Bind each room_code to a webcam-server device and camera_enum once; the device's team and owner are snapshotted at bind time and drive scoped permissions.
  - title: Scheduled recording
    details: A leader-elected scheduler starts every recording before the lesson, retries flaky devices, adopts orphaned upstream streams and stops at the lesson's end.
  - title: Durable ids
    details: The webcam-server stream id and photo id are persisted in PostgreSQL, so recordings stay addressable across restarts; download URLs are minted live and never cached.
  - title: Manual control
    details: Start, stop, switch a camera or take a snapshot mid-session, with X-Idempotency-Key replay protection and an actor audit trail in camera_commands.
  - title: teamusers authorization
    details: Sign-in and permissions stay in teamusers. dispatchub verifies the JWT locally and resolves dispatch:read, dispatch:manage and dispatch:control grants on every request.
---

## What it is

smartclass-dispatchub owns the classroom recording schedule. It is the service between the
academic timetable and the camera fleet:

- **Timetables in.** A term plus its 节次 time table, then a CSV of course/teacher/room rows per
  teaching week. dispatchub validates every row and materializes concrete `sessions`.
- **Recordings out.** For each session the scheduler asks
  [smartclass-webcam-server](https://github.com/crazy4chicken/smartclass-webcam-server) to start
  recording before the lesson and stop it at the end, and stores the returned `stream_id`.
- **Artifacts on demand.** Segments and photos are never mirrored: given a stored `stream_id`,
  dispatchub asks webcam-server for fresh download URLs (15-minute TTL) when a client asks for
  them.
- **Authorization delegated.** All access is authenticated and authorized by
  [teamusers](https://github.com/crazy4chicken/nsc-teamusers); dispatchub keeps no local accounts.

## Quick links

- [Getting Started](/guide/getting-started) - run it locally against PostgreSQL.
- [Deployment](/guide/deploy) - the fleet compose file and the full `DISPATCH_*` reference.
- [Permissions](/guide/permissions) - the nine `dispatch:*` keys and the `any > team > own` ladder.
- [API Usage](/guide/api-usage) - import, schedule, record, fetch artifacts.
- [Operations](/guide/operations) - scheduler semantics, triage and outage behaviour.
- [API Reference](/api/overview) - the generated reference and the OpenAPI document.
