package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/store"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/webcam"
)

// StartSession starts a planned, failed or missed session through
// webcam-server. Replaying an X-Idempotency-Key returns the stored outcome
// without issuing a second upstream command.
func (s *Scheduler) StartSession(ctx context.Context, sessionID, actorID, idempotencyKey string) (domain.Session, error) {
	release := s.keyLocks.lock(idempotencyKey)
	defer release()
	stored, replayed, err := s.lookupCommand(ctx, idempotencyKey)
	if err != nil {
		return domain.Session{}, err
	}
	sess, err := s.store.Sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.Session{}, errSessionNotFound(err)
		}
		return domain.Session{}, err
	}
	if replayed {
		if guardErr := replayGuard(stored, domain.ActionStart, sess.RoomCode, &sess.ID); guardErr != nil {
			return sess, guardErr
		}
		return s.replaySession(ctx, stored)
	}
	room, err := s.store.Rooms.Get(ctx, sess.RoomCode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.Session{}, errRoomNotBound(err)
		}
		return domain.Session{}, err
	}
	if !startable(sess.Status) {
		s.insertAudit(ctx, auditRow{
			SessionID:      &sess.ID,
			Room:           room,
			Action:         domain.ActionStart,
			CameraEnum:     sess.CameraEnum,
			ActorID:        actorID,
			IdempotencyKey: idempotencyKey,
			Outcome:        domain.OutcomeRejected,
			HTTPStatus:     http.StatusConflict,
			Detail:         "session_not_planned",
		})
		return sess, errSessionNotPlanned(store.ErrConflict)
	}

	stream, status, detail, err := s.startUpstream(ctx, sess)
	if err != nil {
		mapped := translateUpstream(err)
		s.insertAudit(ctx, auditRow{
			SessionID:      &sess.ID,
			Room:           room,
			Action:         domain.ActionStart,
			CameraEnum:     sess.CameraEnum,
			ActorID:        actorID,
			IdempotencyKey: idempotencyKey,
			Outcome:        outcomeFor(mapped.Status),
			HTTPStatus:     mapped.Status,
			Detail:         mapped.Detail,
		})
		return sess, mapped
	}
	if rerr := s.recordStart(ctx, sess, stream, status, detail); rerr != nil {
		mapped := recordStartError(rerr)
		s.logger.Error("scheduler: record manual recording start", "session_id", sess.ID, "error", rerr)
		s.insertAudit(ctx, auditRow{
			SessionID:      &sess.ID,
			Room:           room,
			Action:         domain.ActionStart,
			CameraEnum:     sess.CameraEnum,
			ActorID:        actorID,
			IdempotencyKey: idempotencyKey,
			Outcome:        domain.OutcomeFailed,
			HTTPStatus:     mapped.Status,
			Detail:         mapped.Detail,
		})
		return sess, mapped
	}
	s.insertAudit(ctx, auditRow{
		SessionID:      &sess.ID,
		Room:           room,
		Action:         domain.ActionStart,
		CameraEnum:     sess.CameraEnum,
		ActorID:        actorID,
		IdempotencyKey: idempotencyKey,
		Outcome:        domain.OutcomeAccepted,
		HTTPStatus:     status,
		Detail:         detail,
	})
	return s.reloadSession(ctx, sess), nil
}

// StopSession stops a live session. A 404 from the upstream counts as success:
// the stored stream state is reconciled instead.
func (s *Scheduler) StopSession(ctx context.Context, sessionID, actorID, idempotencyKey string) (domain.Session, error) {
	release := s.keyLocks.lock(idempotencyKey)
	defer release()
	stored, replayed, err := s.lookupCommand(ctx, idempotencyKey)
	if err != nil {
		return domain.Session{}, err
	}
	sess, err := s.store.Sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.Session{}, errSessionNotFound(err)
		}
		return domain.Session{}, err
	}
	if replayed {
		if guardErr := replayGuard(stored, domain.ActionStop, sess.RoomCode, &sess.ID); guardErr != nil {
			return sess, guardErr
		}
		return s.replaySession(ctx, stored)
	}
	room, err := s.store.Rooms.Get(ctx, sess.RoomCode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.Session{}, errRoomNotBound(err)
		}
		return domain.Session{}, err
	}
	if !stoppable(sess.Status) {
		s.insertAudit(ctx, auditRow{
			SessionID:      &sess.ID,
			Room:           room,
			Action:         domain.ActionStop,
			CameraEnum:     sess.CameraEnum,
			ActorID:        actorID,
			IdempotencyKey: idempotencyKey,
			Outcome:        domain.OutcomeRejected,
			HTTPStatus:     http.StatusConflict,
			Detail:         "no_active_stream",
		})
		return sess, errNoActiveStream(store.ErrConflict)
	}

	status, stopErr := s.stopUpstream(ctx, sess)
	if stopErr != nil {
		mapped := translateUpstream(stopErr)
		s.insertAudit(ctx, auditRow{
			SessionID:      &sess.ID,
			Room:           room,
			Action:         domain.ActionStop,
			CameraEnum:     sess.CameraEnum,
			ActorID:        actorID,
			IdempotencyKey: idempotencyKey,
			Outcome:        outcomeFor(mapped.Status),
			HTTPStatus:     mapped.Status,
			Detail:         mapped.Detail,
		})
		return sess, mapped
	}
	detail := ""
	if status == http.StatusNotFound {
		detail = "no_active_stream"
	}
	s.insertAudit(ctx, auditRow{
		SessionID:      &sess.ID,
		Room:           room,
		Action:         domain.ActionStop,
		CameraEnum:     sess.CameraEnum,
		ActorID:        actorID,
		IdempotencyKey: idempotencyKey,
		Outcome:        domain.OutcomeAccepted,
		HTTPStatus:     status,
		Detail:         detail,
	})
	return s.reloadSession(ctx, sess), nil
}

// StartAdHoc starts an ad-hoc recording for the room. When the room already
// has a live session that session is adopted instead of creating a second one.
func (s *Scheduler) StartAdHoc(ctx context.Context, roomCode, actorID, idempotencyKey string) (domain.Session, error) {
	release := s.keyLocks.lock(idempotencyKey)
	defer release()
	stored, replayed, err := s.lookupCommand(ctx, idempotencyKey)
	if err != nil {
		return domain.Session{}, err
	}
	room, err := s.store.Rooms.Get(ctx, roomCode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.Session{}, errRoomNotBound(err)
		}
		return domain.Session{}, err
	}
	if replayed {
		if guardErr := replayGuard(stored, domain.ActionStart, roomCode, nil); guardErr != nil {
			return domain.Session{}, guardErr
		}
		return s.replaySession(ctx, stored)
	}

	if active, err := s.store.Sessions.ActiveByRoom(ctx, roomCode); err == nil {
		s.insertAudit(ctx, auditRow{
			SessionID:      &active.ID,
			Room:           room,
			Action:         domain.ActionStart,
			CameraEnum:     active.CameraEnum,
			ActorID:        actorID,
			IdempotencyKey: idempotencyKey,
			Outcome:        domain.OutcomeAccepted,
			HTTPStatus:     http.StatusOK,
			Detail:         "adopted_active_session",
		})
		return active, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return domain.Session{}, err
	}

	now := s.now()
	created, err := s.store.Sessions.CreateManual(ctx, domain.Session{
		RoomCode:   room.RoomCode,
		DeviceID:   room.DeviceID,
		CameraEnum: room.CameraEnum,
		StartsAt:   now,
		EndsAt:     now.Add(manualSessionTTL),
	})
	if err != nil {
		return domain.Session{}, err
	}

	stream, status, detail, err := s.startUpstream(ctx, created)
	if err != nil {
		mapped := translateUpstream(err)
		s.failSession(ctx, created, mapped.Detail)
		s.insertAudit(ctx, auditRow{
			SessionID:      &created.ID,
			Room:           room,
			Action:         domain.ActionStart,
			CameraEnum:     created.CameraEnum,
			ActorID:        actorID,
			IdempotencyKey: idempotencyKey,
			Outcome:        outcomeFor(mapped.Status),
			HTTPStatus:     mapped.Status,
			Detail:         mapped.Detail,
		})
		return created, mapped
	}
	if rerr := s.recordStart(ctx, created, stream, status, detail); rerr != nil {
		mapped := recordStartError(rerr)
		s.logger.Error("scheduler: record ad-hoc recording start", "session_id", created.ID, "error", rerr)
		s.insertAudit(ctx, auditRow{
			SessionID:      &created.ID,
			Room:           room,
			Action:         domain.ActionStart,
			CameraEnum:     created.CameraEnum,
			ActorID:        actorID,
			IdempotencyKey: idempotencyKey,
			Outcome:        domain.OutcomeFailed,
			HTTPStatus:     mapped.Status,
			Detail:         mapped.Detail,
		})
		return created, mapped
	}
	s.insertAudit(ctx, auditRow{
		SessionID:      &created.ID,
		Room:           room,
		Action:         domain.ActionStart,
		CameraEnum:     created.CameraEnum,
		ActorID:        actorID,
		IdempotencyKey: idempotencyKey,
		Outcome:        domain.OutcomeAccepted,
		HTTPStatus:     status,
		Detail:         detail,
	})
	return s.reloadSession(ctx, created), nil
}

// StopAdHoc stops the room's live session. A room without one reports
// no_active_stream.
func (s *Scheduler) StopAdHoc(ctx context.Context, roomCode, actorID, idempotencyKey string) (domain.Session, error) {
	release := s.keyLocks.lock(idempotencyKey)
	defer release()
	stored, replayed, err := s.lookupCommand(ctx, idempotencyKey)
	if err != nil {
		return domain.Session{}, err
	}
	room, err := s.store.Rooms.Get(ctx, roomCode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.Session{}, errRoomNotBound(err)
		}
		return domain.Session{}, err
	}
	if replayed {
		if guardErr := replayGuard(stored, domain.ActionStop, roomCode, nil); guardErr != nil {
			return domain.Session{}, guardErr
		}
		return s.replaySession(ctx, stored)
	}

	active, err := s.store.Sessions.ActiveByRoom(ctx, roomCode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.insertAudit(ctx, auditRow{
				Room:           room,
				Action:         domain.ActionStop,
				CameraEnum:     room.CameraEnum,
				ActorID:        actorID,
				IdempotencyKey: idempotencyKey,
				Outcome:        domain.OutcomeRejected,
				HTTPStatus:     http.StatusNotFound,
				Detail:         "no_active_stream",
			})
			return domain.Session{}, errNoActiveStream(err)
		}
		return domain.Session{}, err
	}

	status, stopErr := s.stopUpstream(ctx, active)
	if stopErr != nil {
		mapped := translateUpstream(stopErr)
		s.insertAudit(ctx, auditRow{
			SessionID:      &active.ID,
			Room:           room,
			Action:         domain.ActionStop,
			CameraEnum:     active.CameraEnum,
			ActorID:        actorID,
			IdempotencyKey: idempotencyKey,
			Outcome:        outcomeFor(mapped.Status),
			HTTPStatus:     mapped.Status,
			Detail:         mapped.Detail,
		})
		return active, mapped
	}
	detail := ""
	if status == http.StatusNotFound {
		detail = "no_active_stream"
	}
	s.insertAudit(ctx, auditRow{
		SessionID:      &active.ID,
		Room:           room,
		Action:         domain.ActionStop,
		CameraEnum:     active.CameraEnum,
		ActorID:        actorID,
		IdempotencyKey: idempotencyKey,
		Outcome:        domain.OutcomeAccepted,
		HTTPStatus:     status,
		Detail:         detail,
	})
	return s.reloadSession(ctx, active), nil
}

// SwitchCamera queues a camera switch on the room's live session.
func (s *Scheduler) SwitchCamera(ctx context.Context, roomCode string, cameraEnum int, actorID, idempotencyKey string) (webcam.CommandAck, error) {
	release := s.keyLocks.lock(idempotencyKey)
	defer release()
	stored, replayed, err := s.lookupCommand(ctx, idempotencyKey)
	if err != nil {
		return webcam.CommandAck{}, err
	}
	room, err := s.store.Rooms.Get(ctx, roomCode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return webcam.CommandAck{}, errRoomNotBound(err)
		}
		return webcam.CommandAck{}, err
	}
	if replayed {
		if guardErr := replayGuard(stored, domain.ActionSwitch, roomCode, nil); guardErr != nil {
			return webcam.CommandAck{}, guardErr
		}
		if stored.Outcome != domain.OutcomeAccepted {
			return webcam.CommandAck{}, replayError(stored)
		}
		return ackFromCommand(stored), nil
	}

	active, err := s.store.Sessions.ActiveByRoom(ctx, roomCode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.insertAudit(ctx, auditRow{
				Room:           room,
				Action:         domain.ActionSwitch,
				CameraEnum:     cameraEnum,
				ActorID:        actorID,
				IdempotencyKey: idempotencyKey,
				Outcome:        domain.OutcomeRejected,
				HTTPStatus:     http.StatusNotFound,
				Detail:         "no_active_stream",
			})
			return webcam.CommandAck{}, errNoActiveStream(err)
		}
		return webcam.CommandAck{}, err
	}

	var ack webcam.CommandAck
	callErr := s.withSlot(ctx, func(ctx context.Context) error {
		queued, err := s.webcam.Switch(ctx, room.DeviceID, cameraEnum)
		if err != nil {
			return err
		}
		ack = queued
		return nil
	})
	if callErr != nil {
		mapped := translateUpstream(callErr)
		s.insertAudit(ctx, auditRow{
			SessionID:      &active.ID,
			Room:           room,
			Action:         domain.ActionSwitch,
			CameraEnum:     cameraEnum,
			ActorID:        actorID,
			IdempotencyKey: idempotencyKey,
			Outcome:        outcomeFor(mapped.Status),
			HTTPStatus:     mapped.Status,
			Detail:         mapped.Detail,
		})
		return webcam.CommandAck{}, mapped
	}
	s.insertAudit(ctx, auditRow{
		SessionID:      &active.ID,
		Room:           room,
		Action:         domain.ActionSwitch,
		CameraEnum:     cameraEnum,
		ActorID:        actorID,
		IdempotencyKey: idempotencyKey,
		Outcome:        domain.OutcomeAccepted,
		HTTPStatus:     http.StatusAccepted,
		CommandID:      ack.CommandID,
	})
	return ack, nil
}

// CapturePhoto queues a snapshot on the room's camera and records the capture
// in the photo ledger. Snapshotting is legal while idle too: the ledger row
// then carries no session. The photo id itself is resolved asynchronously by
// the scheduler, matched on the returned request id.
func (s *Scheduler) CapturePhoto(ctx context.Context, sessionID *string, roomCode, actorID, idempotencyKey string) (domain.Photo, error) {
	release := s.keyLocks.lock(idempotencyKey)
	defer release()
	stored, replayed, err := s.lookupCommand(ctx, idempotencyKey)
	if err != nil {
		return domain.Photo{}, err
	}
	room, err := s.store.Rooms.Get(ctx, roomCode)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.Photo{}, errRoomNotBound(err)
		}
		return domain.Photo{}, err
	}
	if replayed {
		if guardErr := replayGuard(stored, domain.ActionPhoto, roomCode, sessionID); guardErr != nil {
			return domain.Photo{}, guardErr
		}
		return s.replayPhoto(ctx, stored)
	}

	var sess *domain.Session
	if sessionID != nil {
		found, err := s.store.Sessions.Get(ctx, *sessionID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return domain.Photo{}, errSessionNotFound(err)
			}
			return domain.Photo{}, err
		}
		if found.RoomCode != roomCode {
			return domain.Photo{}, errInternal(fmt.Errorf("session %s belongs to room %s, not %s", found.ID, found.RoomCode, roomCode))
		}
		sess = &found
	} else if active, err := s.store.Sessions.ActiveByRoom(ctx, roomCode); err == nil {
		sess = &active
	} else if !errors.Is(err, store.ErrNotFound) {
		return domain.Photo{}, err
	}

	var ack webcam.CommandAck
	callErr := s.withSlot(ctx, func(ctx context.Context) error {
		queued, err := s.webcam.TakePhoto(ctx, room.DeviceID, room.CameraEnum)
		if err != nil {
			return err
		}
		ack = queued
		return nil
	})
	if callErr != nil {
		mapped := translateUpstream(callErr)
		s.insertAudit(ctx, auditRow{
			SessionID:      sessionIDOf(sess),
			Room:           room,
			Action:         domain.ActionPhoto,
			CameraEnum:     room.CameraEnum,
			ActorID:        actorID,
			IdempotencyKey: idempotencyKey,
			Outcome:        outcomeFor(mapped.Status),
			HTTPStatus:     mapped.Status,
			Detail:         mapped.Detail,
		})
		return domain.Photo{}, mapped
	}

	photo := domain.Photo{
		RoomCode:   room.RoomCode,
		DeviceID:   room.DeviceID,
		CameraEnum: room.CameraEnum,
		RequestID:  ack.RequestID,
		Source:     domain.PhotoSourceManual,
		Status:     domain.PhotoPending,
		TakenAt:    s.now(),
	}
	if sess != nil {
		photo.SessionID = &sess.ID
		photo.Source = photoSourceOf(sess.Origin)
	}
	if actorID != "" {
		photo.ActorID = &actorID
	}
	if ack.RequestID == "" {
		// Without a request id the photo can never be matched; keep the row as
		// an honest record instead of polling for it forever.
		photo.Status = domain.PhotoUnresolved
	}
	storedPhoto, err := s.store.Photos.Create(ctx, photo)
	if err != nil {
		return domain.Photo{}, err
	}
	s.insertAudit(ctx, auditRow{
		SessionID:      sessionIDOf(sess),
		Room:           room,
		Action:         domain.ActionPhoto,
		CameraEnum:     room.CameraEnum,
		ActorID:        actorID,
		IdempotencyKey: idempotencyKey,
		Outcome:        domain.OutcomeAccepted,
		HTTPStatus:     http.StatusAccepted,
		RequestID:      ack.RequestID,
		CommandID:      ack.CommandID,
	})
	return storedPhoto, nil
}

// auditRow is one camera_commands row to write.
type auditRow struct {
	SessionID      *string
	Room           domain.Room
	Action         string
	CameraEnum     int
	ActorID        string
	IdempotencyKey string
	Outcome        string
	HTTPStatus     int
	Detail         string
	RequestID      string
	CommandID      string
}

// insertAudit writes the audit row for one attempted control action. Exactly
// one row is written per attempt; a write failure is logged, never returned, so
// an audit problem cannot turn a completed command into an error.
func (s *Scheduler) insertAudit(ctx context.Context, row auditRow) {
	cmd := domain.Command{
		SessionID:  row.SessionID,
		RoomCode:   row.Room.RoomCode,
		DeviceID:   row.Room.DeviceID,
		Action:     row.Action,
		CameraEnum: row.CameraEnum,
		ActorID:    row.ActorID,
		Outcome:    row.Outcome,
	}
	if row.IdempotencyKey != "" {
		key := row.IdempotencyKey
		cmd.IdempotencyKey = &key
	}
	if row.HTTPStatus != 0 {
		status := row.HTTPStatus
		cmd.HTTPStatus = &status
	}
	if row.Detail != "" {
		detail := row.Detail
		cmd.Detail = &detail
	}
	if row.RequestID != "" {
		requestID := row.RequestID
		cmd.RequestID = &requestID
	}
	if row.CommandID != "" {
		commandID := row.CommandID
		cmd.CommandID = &commandID
	}
	if _, _, err := s.saveCommand(ctx, cmd); err != nil {
		s.logger.Error("scheduler: write command audit row",
			"action", cmd.Action, "room_code", cmd.RoomCode, "error", err)
	}
}

// keyLocks serializes commands that share one X-Idempotency-Key. The audit row
// that stores the outcome is only written after the upstream command, so
// without this lock two concurrent replays would both miss it and both reach
// webcam-server. The map holds one entry per key in flight and drops it when
// the last holder releases, so it never grows with the number of keys seen.
type keyLocks struct {
	mu    sync.Mutex
	locks map[string]*keyLock
}

// keyLock is one in-flight idempotency key: a mutex plus the number of holders
// and waiters that keep its map entry alive.
type keyLock struct {
	mu   sync.Mutex
	refs int
}

// newKeyLocks builds an empty key lock table.
func newKeyLocks() *keyLocks {
	return &keyLocks{locks: make(map[string]*keyLock)}
}

// lock serializes the caller against every other command carrying the same
// key and returns its release function. An empty key is never stored in
// camera_commands, so it is never serialized.
func (k *keyLocks) lock(key string) func() {
	if key == "" {
		return func() {}
	}
	k.mu.Lock()
	entry := k.locks[key]
	if entry == nil {
		entry = &keyLock{}
		k.locks[key] = entry
	}
	entry.refs++
	k.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// lookupCommand returns the command stored under an idempotency key. An empty
// key, or a key that was never used, reports false so the caller proceeds.
func (s *Scheduler) lookupCommand(ctx context.Context, key string) (domain.Command, bool, error) {
	if key == "" {
		return domain.Command{}, false, nil
	}
	cmd, err := s.store.Commands.ByIdempotencyKey(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Command{}, false, nil
	}
	if err != nil {
		return domain.Command{}, false, fmt.Errorf("scheduler: look up idempotency key: %w", err)
	}
	return cmd, true, nil
}

// saveCommand stores one audit row. When another request already stored the
// same idempotency key it returns that row instead (replayed=true), which is
// the race-safe half of the idempotency contract.
func (s *Scheduler) saveCommand(ctx context.Context, cmd domain.Command) (domain.Command, bool, error) {
	stored, err := s.store.Commands.Insert(ctx, cmd)
	if err == nil {
		return stored, false, nil
	}
	if errors.Is(err, store.ErrConflict) && cmd.IdempotencyKey != nil {
		existing, lookupErr := s.store.Commands.ByIdempotencyKey(ctx, *cmd.IdempotencyKey)
		if lookupErr == nil {
			return existing, true, nil
		}
	}
	return domain.Command{}, false, err
}

// replaySession answers a replayed start or stop with the session it produced,
// or with the error it was rejected with.
func (s *Scheduler) replaySession(ctx context.Context, cmd domain.Command) (domain.Session, error) {
	if cmd.SessionID == nil {
		return domain.Session{}, replayError(cmd)
	}
	sess, err := s.store.Sessions.Get(ctx, *cmd.SessionID)
	if err != nil {
		return domain.Session{}, err
	}
	if cmd.Outcome == domain.OutcomeAccepted {
		return sess, nil
	}
	return sess, replayError(cmd)
}

// replayPhoto answers a replayed snapshot with the ledger row it produced.
func (s *Scheduler) replayPhoto(ctx context.Context, cmd domain.Command) (domain.Photo, error) {
	if cmd.Outcome != domain.OutcomeAccepted {
		return domain.Photo{}, replayError(cmd)
	}
	if cmd.RequestID != nil {
		photo, err := s.store.Photos.ByRequestID(ctx, *cmd.RequestID)
		if err == nil {
			return photo, nil
		}
	}
	return domain.Photo{}, errInternal(fmt.Errorf("replayed photo command %s has no ledger row", cmd.ID))
}

// replayError reconstructs the error a stored command outcome represented.
func replayError(cmd domain.Command) *Error {
	status := http.StatusServiceUnavailable
	if cmd.HTTPStatus != nil {
		status = *cmd.HTTPStatus
	}
	detail := "upstream_unavailable"
	if cmd.Detail != nil && *cmd.Detail != "" {
		detail = *cmd.Detail
	}
	var cause error
	if cmd.Outcome == domain.OutcomeRejected {
		cause = store.ErrConflict
	} else {
		cause = &webcam.UpstreamError{Status: status, Detail: detail}
	}
	return &Error{Status: status, Detail: detail, Err: cause}
}

// ackFromCommand rebuilds the command acknowledgement stored for a switch.
func ackFromCommand(cmd domain.Command) webcam.CommandAck {
	ack := webcam.CommandAck{CameraEnum: cmd.CameraEnum}
	if cmd.CommandID != nil {
		ack.CommandID = *cmd.CommandID
	}
	return ack
}

// startable reports whether a session may be started manually.
func startable(status string) bool {
	switch status {
	case domain.SessionPlanned, domain.SessionFailed, domain.SessionMissed:
		return true
	default:
		return false
	}
}

// stoppable reports whether a session has a live recording to stop.
func stoppable(status string) bool {
	switch status {
	case domain.SessionStarting, domain.SessionRecording, domain.SessionStopping:
		return true
	default:
		return false
	}
}

// recordStartError maps a failed write of a started upstream recording onto
// the control error contract: the store refusing the state transition is a
// conflict, anything else is an internal error. The upstream recording is live
// in both cases, so the caller must never answer success.
func recordStartError(err error) *Error {
	if errors.Is(err, store.ErrConflict) {
		return errSessionNotPlanned(err)
	}
	return errInternal(err)
}

// photoSourceOf maps a session origin onto the session_photos.source
// vocabulary: the column admits scheduled|manual, while sessions.origin stores
// schedule|manual.
func photoSourceOf(origin string) string {
	if origin == domain.OriginSchedule {
		return domain.PhotoSourceScheduled
	}
	return domain.PhotoSourceManual
}

// sessionIDOf renders the optional session id of an audit row.
func sessionIDOf(sess *domain.Session) *string {
	if sess == nil {
		return nil
	}
	id := sess.ID
	return &id
}

// reloadSession returns the session as it is stored now, falling back to the
// passed value when the read fails.
func (s *Scheduler) reloadSession(ctx context.Context, sess domain.Session) domain.Session {
	updated, err := s.store.Sessions.Get(ctx, sess.ID)
	if err != nil {
		return sess
	}
	return updated
}
