// Package scheduler drives the recording schedule. It owns advisory-lock
// leader election, the claim/start/stop tick, upstream reconciliation via the
// watchdog, missed-session housekeeping, the bounded outbound command
// concurrency and the asynchronous photo-id resolver.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/config"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/store"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/webcam"
)

// Deps assembles a Scheduler. Clock defaults to time.Now and Logger to
// slog.Default.
type Deps struct {
	Store  *store.Store
	Webcam *webcam.Client
	Config config.Config
	Logger *slog.Logger
	Clock  func() time.Time // nil = time.Now
}

// Scheduler is the recording state machine shared by the background loop and
// the HTTP control commands.
type Scheduler struct {
	store    *store.Store
	webcam   *webcam.Client
	cfg      config.Config
	logger   *slog.Logger
	clock    func() time.Time
	slots    *commandPool
	keyLocks *keyLocks
}

// Batching and pacing constants.
const (
	watchBatch             = 64            // sessions reconciled per tick
	photoClaimBatch        = 64            // pending photos claimed per resolver pass
	photoLookupBase        = 20            // upstream photo list size floor
	photoLookupMax         = 200           // upstream photo list size ceiling
	manualSessionTTL       = 4 * time.Hour // ad-hoc sessions have no scheduled end
	startBackoffShift      = 4             // start retry backoff caps at 16 ticks
	photoBackoffShift      = 6             // photo poll backoff caps at 64 intervals
	adoptStreamLookupLimit = 50            // device streams scanned when adopting
)

// New builds a Scheduler. It never reaches the network.
func New(deps Deps) *Scheduler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	cfg := deps.Config
	// Defensive defaults: main validates the configuration, but New must not
	// be able to build a scheduler that panics on a zero ticker.
	if cfg.SchedTick <= 0 {
		cfg.SchedTick = 30 * time.Second
	}
	if cfg.PhotoPollInterval <= 0 {
		cfg.PhotoPollInterval = 2 * time.Second
	}
	if cfg.PhotoPollTTL <= 0 {
		cfg.PhotoPollTTL = 15 * time.Minute
	}
	if cfg.WebcamTimeout <= 0 {
		cfg.WebcamTimeout = 10 * time.Second
	}
	if cfg.MaxConcurrentCommands < 1 {
		cfg.MaxConcurrentCommands = 1
	}
	return &Scheduler{
		store:    deps.Store,
		webcam:   deps.Webcam,
		cfg:      cfg,
		logger:   logger,
		clock:    clock,
		slots:    newCommandPool(cfg.MaxConcurrentCommands),
		keyLocks: newKeyLocks(),
	}
}

// Run blocks until ctx is cancelled. It acquires the dispatchub leader lock
// first, so every extra replica idles instead of splitting the schedule, then
// runs the tick loop and one photo-resolver goroutine. The lock is released
// when Run returns.
func (s *Scheduler) Run(ctx context.Context) error {
	conn, err := s.acquireLeadership(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		return err
	}
	defer s.releaseLeadership(conn)
	s.logger.Info("scheduler: leadership acquired",
		"lock_key", leaderLockKey,
		"tick", s.cfg.SchedTick.String(),
		"max_concurrent_commands", s.cfg.MaxConcurrentCommands,
	)

	var resolver sync.WaitGroup
	resolver.Add(1)
	go func() {
		defer resolver.Done()
		s.runPhotoResolver(ctx)
	}()

	s.tick(ctx)
	ticker := time.NewTicker(s.cfg.SchedTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			resolver.Wait()
			s.logger.Info("scheduler: stopped")
			return nil
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// now reads the injected clock.
func (s *Scheduler) now() time.Time { return s.clock() }

// batchSize is the per-tick claim batch: one session per outbound command
// slot, so a tick never queues more work than the pool can run.
func (s *Scheduler) batchSize() int {
	if s.cfg.MaxConcurrentCommands < 1 {
		return 1
	}
	return s.cfg.MaxConcurrentCommands
}

// tick performs one claim, act and reconcile pass.
func (s *Scheduler) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := s.now()
	batch := s.batchSize()

	due, err := s.store.Sessions.ClaimDue(ctx, now, s.cfg.Prestart, batch)
	if err != nil {
		s.logger.Error("scheduler: claim due sessions", "error", err)
		return
	}
	expired, err := s.store.Sessions.ClaimExpired(ctx, now, s.cfg.PoststopGrace, batch)
	if err != nil {
		s.logger.Error("scheduler: claim expired recordings", "error", err)
		return
	}

	jobs := make([]func(context.Context), 0, len(due)+len(expired))
	claimed := make(map[string]struct{}, len(due)+len(expired))
	for _, sess := range due {
		claimed[sess.ID] = struct{}{}
		jobs = append(jobs, s.startJob(sess))
	}
	for _, sess := range expired {
		claimed[sess.ID] = struct{}{}
		jobs = append(jobs, s.stopJob(sess))
	}

	// Reconciliation backlog: retry starts whose backoff has elapsed, retry
	// stops that never completed and poll recordings against the upstream
	// state. webcam-server never sends events, so polling is the only defence
	// against a stream that ended behind dispatchub's back.
	active, err := s.store.Sessions.ListRecording(ctx, watchBatch)
	if err != nil {
		s.logger.Error("scheduler: list active sessions", "error", err)
	} else {
		for _, sess := range active {
			if _, ok := claimed[sess.ID]; ok {
				continue
			}
			switch sess.Status {
			case domain.SessionStarting:
				switch {
				case sess.RetryCount >= s.cfg.StartRetryMax:
					jobs = append(jobs, s.failJob(sess, lastErrorOr(sess.LastError, "start retries exhausted")))
				case s.retryDue(sess, now):
					jobs = append(jobs, s.startJob(sess))
				}
			case domain.SessionStopping:
				jobs = append(jobs, s.stopJob(sess))
			case domain.SessionRecording:
				// Give a freshly started recording one tick before the first
				// upstream poll.
				if now.Sub(sess.UpdatedAt) >= s.cfg.SchedTick {
					jobs = append(jobs, s.watchJob(sess))
				}
			}
		}
	}

	s.dispatch(ctx, jobs)

	missed, err := s.store.Sessions.MarkMissed(ctx, now.Add(-s.cfg.MissGrace))
	if err != nil {
		s.logger.Error("scheduler: mark missed sessions", "error", err)
	} else if missed > 0 {
		s.logger.Warn("scheduler: planned sessions missed", "count", missed, "grace", s.cfg.MissGrace.String())
	}
}

// startJob starts or retries one session.
func (s *Scheduler) startJob(sess domain.Session) func(context.Context) {
	return func(ctx context.Context) { s.runStart(ctx, sess) }
}

// stopJob stops one recording.
func (s *Scheduler) stopJob(sess domain.Session) func(context.Context) {
	return func(ctx context.Context) { s.runStop(ctx, sess) }
}

// watchJob polls one live session against the upstream stream state.
func (s *Scheduler) watchJob(sess domain.Session) func(context.Context) {
	return func(ctx context.Context) { s.runWatch(ctx, sess) }
}

// failJob terminates one session without an upstream call.
func (s *Scheduler) failJob(sess domain.Session, detail string) func(context.Context) {
	return func(ctx context.Context) { s.failSession(ctx, sess, detail) }
}

// runStart performs one start attempt. A session whose window already closed is
// failed instead: recording a lesson that ended is worse than reporting it.
func (s *Scheduler) runStart(ctx context.Context, sess domain.Session) {
	if !sess.EndsAt.After(s.now()) {
		s.failSession(ctx, sess, "session ended before recording started")
		return
	}
	stream, status, detail, err := s.startUpstream(ctx, sess)
	if err != nil {
		s.retryOrFail(ctx, sess, err)
		return
	}
	if err := s.recordStart(ctx, sess, stream, status, detail); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// The store refused the transition even though the upstream camera
			// is now streaming: another session adopted the stream. Failing
			// the row stops the retry loop, which would otherwise re-adopt the
			// same stream on every tick forever.
			if current := s.reloadSession(ctx, sess); current.Status == domain.SessionStarting {
				s.failSession(ctx, sess, "stream adopted by another session")
				return
			}
			s.logger.Warn("scheduler: session left starting before the start was recorded",
				"session_id", sess.ID, "stream_id", stream.ID)
			return
		}
		s.logger.Error("scheduler: record recording start", "session_id", sess.ID, "error", err)
	}
}

// startUpstream starts the session's camera and adopts an active upstream
// stream when webcam-server reports the camera as already streaming (the
// recovery path for orphaned streams left by a webcam-server crash). It
// reports the upstream HTTP status and, for an adoption, the stable detail.
func (s *Scheduler) startUpstream(ctx context.Context, sess domain.Session) (webcam.Stream, int, string, error) {
	var (
		stream webcam.Stream
		status = http.StatusCreated
		detail string
	)
	err := s.withSlot(ctx, func(ctx context.Context) error {
		started, err := s.webcam.Start(ctx, sess.DeviceID, sess.CameraEnum)
		if err == nil {
			stream = started
			return nil
		}
		if webcam.IsStatus(err, http.StatusConflict) && isAlreadyStreaming(err) {
			adopted, adoptErr := s.adoptStream(ctx, sess)
			if adoptErr != nil {
				return adoptErr
			}
			stream, status, detail = adopted, http.StatusConflict, "already_streaming"
			return nil
		}
		return err
	})
	if err != nil {
		return webcam.Stream{}, status, detail, err
	}
	return stream, status, detail, nil
}

// adoptStream finds the active upstream stream of the session's camera.
func (s *Scheduler) adoptStream(ctx context.Context, sess domain.Session) (webcam.Stream, error) {
	streams, err := s.webcam.Streams(ctx, sess.DeviceID, adoptStreamLookupLimit)
	if err != nil {
		return webcam.Stream{}, err
	}
	for _, stream := range streams {
		if stream.CameraEnum == sess.CameraEnum && stream.Status == webcam.StreamActive {
			return stream, nil
		}
	}
	return webcam.Stream{}, &webcam.UpstreamError{
		Status: http.StatusConflict,
		Detail: "camera reported as already streaming but no active stream was listed",
	}
}

// recordStart stores the upstream stream handle and moves the session to
// recording.
func (s *Scheduler) recordStart(ctx context.Context, sess domain.Session, stream webcam.Stream, status int, detail string) error {
	startedAt := stream.StartedAt
	if startedAt.IsZero() {
		startedAt = s.now()
	}
	if err := s.store.Sessions.MarkRecording(ctx, sess.ID, stream.ID, startedAt); err != nil {
		return err
	}
	s.logger.Info("scheduler: recording started",
		"session_id", sess.ID,
		"room_code", sess.RoomCode,
		"device_id", sess.DeviceID,
		"stream_id", stream.ID,
		"http_status", status,
		"upstream_detail", detail,
	)
	return nil
}

// retryOrFail records one failed start attempt. Retryable failures keep the
// session in starting and are retried with exponential backoff, measured in
// scheduler ticks, until StartRetryMax attempts have been used; every other
// failure terminates the session with the upstream detail.
func (s *Scheduler) retryOrFail(ctx context.Context, sess domain.Session, err error) {
	detail := upstreamDetail(err)
	if !startRetryable(err) {
		s.failSession(ctx, sess, detail)
		return
	}
	count, cerr := s.store.Sessions.BumpRetry(ctx, sess.ID, detail)
	if cerr != nil {
		s.logger.Error("scheduler: record start retry", "session_id", sess.ID, "error", cerr)
		return
	}
	if count >= s.cfg.StartRetryMax {
		s.failSession(ctx, sess, detail)
		return
	}
	s.logger.Warn("scheduler: recording start failed, retrying",
		"session_id", sess.ID,
		"room_code", sess.RoomCode,
		"retry_count", count,
		"max_attempts", s.cfg.StartRetryMax,
		"upstream_detail", detail,
		"next_attempt_in", s.startBackoff(count).String(),
	)
}

// failSession terminates a session and logs the reason.
func (s *Scheduler) failSession(ctx context.Context, sess domain.Session, detail string) {
	if err := s.store.Sessions.MarkFailed(ctx, sess.ID, detail); err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.logger.Warn("scheduler: session left the startable state before it could be failed",
				"session_id", sess.ID, "upstream_detail", detail)
			return
		}
		s.logger.Error("scheduler: mark session failed", "session_id", sess.ID, "error", err)
		return
	}
	s.logger.Warn("scheduler: session failed",
		"session_id", sess.ID,
		"room_code", sess.RoomCode,
		"upstream_detail", detail,
	)
}

// completeSession finishes a session and logs the reason.
func (s *Scheduler) completeSession(ctx context.Context, sess domain.Session, endedAt time.Time, reason string) {
	if err := s.store.Sessions.MarkCompleted(ctx, sess.ID, endedAt); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return
		}
		s.logger.Error("scheduler: mark session completed", "session_id", sess.ID, "error", err)
		return
	}
	s.logger.Info("scheduler: session completed",
		"session_id", sess.ID,
		"room_code", sess.RoomCode,
		"reason", reason,
	)
}

// runStop stops one recording.
func (s *Scheduler) runStop(ctx context.Context, sess domain.Session) {
	status, err := s.stopUpstream(ctx, sess)
	if err != nil {
		s.logger.Warn("scheduler: recording stop failed, will retry",
			"session_id", sess.ID,
			"room_code", sess.RoomCode,
			"http_status", status,
			"upstream_detail", webcam.DetailOf(err),
		)
		return
	}
	s.logger.Info("scheduler: recording stopped",
		"session_id", sess.ID,
		"room_code", sess.RoomCode,
		"http_status", status,
	)
}

// stopUpstream stops the session's recording and reconciles the local row. A
// 404 "no active stream" counts as success — the upstream stream already
// finished — and the stored stream's terminal state is mirrored when it is
// still reachable. Failures keep the session recording so the watchdog and the
// next tick keep tracking the live stream. The upstream HTTP status is
// returned for the audit trail.
func (s *Scheduler) stopUpstream(ctx context.Context, sess domain.Session) (int, error) {
	var (
		status    = http.StatusOK
		endedAt   = s.now()
		failedWhy string
	)
	err := s.withSlot(ctx, func(ctx context.Context) error {
		stream, err := s.webcam.Stop(ctx, sess.DeviceID, sess.CameraEnum)
		if err == nil {
			if stream.EndedAt != nil {
				endedAt = *stream.EndedAt
			}
			return nil
		}
		if !webcam.IsStatus(err, http.StatusNotFound) || !isNoActiveStream(err) {
			status = upstreamStatus(err)
			return err
		}
		status = http.StatusNotFound
		if sess.StreamID == nil {
			return nil
		}
		detail, detailErr := s.webcam.StreamDetail(ctx, *sess.StreamID)
		if detailErr != nil {
			// The stop answer is authoritative: upstream has no active stream,
			// so the recording is over even when the row is unreadable.
			return nil
		}
		switch detail.Status {
		case webcam.StreamFailed:
			failedWhy = "upstream stream failed"
		case webcam.StreamCompleted:
			if detail.EndedAt != nil {
				endedAt = *detail.EndedAt
			}
		}
		return nil
	})
	if err != nil {
		// Back to recording: the stream is still live upstream.
		if markErr := s.store.Sessions.MarkStopped(ctx, sess.ID); markErr != nil && !errors.Is(markErr, store.ErrConflict) {
			s.logger.Error("scheduler: keep session recording after a failed stop", "session_id", sess.ID, "error", markErr)
		}
		return status, err
	}
	if failedWhy != "" {
		if markErr := s.store.Sessions.MarkFailed(ctx, sess.ID, failedWhy); markErr != nil && !errors.Is(markErr, store.ErrConflict) {
			return status, markErr
		}
		return status, nil
	}
	if markErr := s.store.Sessions.MarkCompleted(ctx, sess.ID, endedAt); markErr != nil && !errors.Is(markErr, store.ErrConflict) {
		return status, markErr
	}
	return status, nil
}

// runWatch mirrors the upstream state of one live session.
func (s *Scheduler) runWatch(ctx context.Context, sess domain.Session) {
	if sess.StreamID == nil {
		return
	}
	// A hung upstream must not stall the tick: the poll gets one upstream
	// timeout in total (a refused connection still retries inside it), and the
	// next tick polls again.
	pollCtx, cancel := context.WithTimeout(ctx, s.cfg.WebcamTimeout)
	defer cancel()

	var detail webcam.StreamDetail
	err := s.withSlot(pollCtx, func(ctx context.Context) error {
		upstream, err := s.webcam.StreamDetail(ctx, *sess.StreamID)
		if err != nil {
			return err
		}
		detail = upstream
		return nil
	})
	if err != nil {
		if webcam.IsStatus(err, http.StatusNotFound) {
			// The stream vanished upstream: a stopping session is finished, a
			// recording one cannot be reconciled and is failed.
			if sess.Status == domain.SessionStopping {
				s.completeSession(ctx, sess, s.now(), "stream_not_found")
				return
			}
			s.failSession(ctx, sess, "upstream stream not found")
			return
		}
		s.logger.Debug("scheduler: watchdog poll failed", "session_id", sess.ID, "error", err)
		return
	}
	switch detail.Status {
	case webcam.StreamCompleted:
		endedAt := s.now()
		if detail.EndedAt != nil {
			endedAt = *detail.EndedAt
		}
		s.completeSession(ctx, sess, endedAt, "upstream_completed")
	case webcam.StreamFailed:
		s.failSession(ctx, sess, "upstream stream failed")
	}
}

// retryDue reports whether a starting session's next attempt is due. The
// backoff doubles with every failed attempt and is measured in ticks, so a
// retry survives a process restart instead of living in a sleep.
func (s *Scheduler) retryDue(sess domain.Session, now time.Time) bool {
	if sess.RetryCount <= 0 {
		return true
	}
	return !now.Before(sess.UpdatedAt.Add(s.startBackoff(sess.RetryCount)))
}

// startBackoff is the pause before retry number failures+1.
func (s *Scheduler) startBackoff(failures int) time.Duration {
	shift := failures - 1
	if shift < 0 {
		shift = 0
	}
	if shift > startBackoffShift {
		shift = startBackoffShift
	}
	return s.cfg.SchedTick * time.Duration(1<<shift)
}

// startRetryable reports whether a failed start is worth another attempt.
// Device-offline and already-streaming conflicts, upstream 5xx answers and
// transport failures (no response) all may recover; other 4xx answers will not.
func startRetryable(err error) bool {
	var upstream *webcam.UpstreamError
	if errors.As(err, &upstream) {
		if upstream.Status == http.StatusConflict {
			return true
		}
		return upstream.Status >= http.StatusInternalServerError
	}
	return true
}

// lastErrorOr renders a session's stored last_error, or fallback when absent.
func lastErrorOr(lastError *string, fallback string) string {
	if lastError != nil && *lastError != "" {
		return *lastError
	}
	return fallback
}

// runPhotoResolver polls webcam-server for the ids of pending captures. It is
// deliberately one goroutine: an upstream outage degrades into growing
// next_poll_at delays instead of a hot loop.
func (s *Scheduler) runPhotoResolver(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PhotoPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.resolvePhotos(ctx)
		}
	}
}

// resolvePhotos claims one bounded batch of due photos and resolves it.
func (s *Scheduler) resolvePhotos(ctx context.Context) {
	now := s.now()
	pending, err := s.store.Photos.ClaimPending(ctx, now, photoClaimBatch)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("scheduler: claim pending photos", "error", err)
		}
		return
	}
	if len(pending) == 0 {
		return
	}

	// One upstream list call per device covers every pending capture on it.
	grouped := make(map[string][]domain.Photo, len(pending))
	order := make([]string, 0, len(pending))
	for _, photo := range pending {
		if _, ok := grouped[photo.DeviceID]; !ok {
			order = append(order, photo.DeviceID)
		}
		grouped[photo.DeviceID] = append(grouped[photo.DeviceID], photo)
	}
	for _, deviceID := range order {
		if ctx.Err() != nil {
			return
		}
		s.resolveDevicePhotos(ctx, deviceID, grouped[deviceID], now)
	}
}

// resolveDevicePhotos matches a device's pending request ids against its
// newest photos and resolves, defers or gives up on each ledger row.
func (s *Scheduler) resolveDevicePhotos(ctx context.Context, deviceID string, pending []domain.Photo, now time.Time) {
	limit := photoLookupBase + 2*len(pending)
	if limit > photoLookupMax {
		limit = photoLookupMax
	}

	var photos []webcam.Photo
	err := s.withSlot(ctx, func(ctx context.Context) error {
		list, err := s.webcam.Photos(ctx, deviceID, limit)
		if err != nil {
			return err
		}
		photos = list
		return nil
	})
	if err != nil {
		// Upstream unavailable: every pending capture backs off.
		for _, photo := range pending {
			s.deferPhoto(ctx, photo, now)
		}
		return
	}

	byRequest := make(map[string]string, len(photos))
	for _, photo := range photos {
		if photo.RequestID != nil && *photo.RequestID != "" {
			byRequest[*photo.RequestID] = photo.ID
		}
	}
	for _, photo := range pending {
		upstreamID, ok := byRequest[photo.RequestID]
		if !ok {
			s.deferPhoto(ctx, photo, now)
			continue
		}
		if err := s.store.Photos.Resolve(ctx, photo.ID, upstreamID, now); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				s.logger.Error("scheduler: resolve photo", "photo_id", photo.ID, "error", err)
			}
			continue
		}
		s.logger.Info("scheduler: photo resolved",
			"photo_id", photo.ID,
			"room_code", photo.RoomCode,
			"upstream_photo_id", upstreamID,
			"attempts", photo.Attempts+1,
		)
	}
}

// deferPhoto reschedules a pending photo with exponential backoff and gives up
// once the poll TTL has passed.
func (s *Scheduler) deferPhoto(ctx context.Context, photo domain.Photo, now time.Time) {
	if now.Sub(photo.TakenAt) >= s.cfg.PhotoPollTTL {
		if err := s.store.Photos.MarkUnresolved(ctx, photo.ID); err != nil {
			s.logger.Error("scheduler: mark photo unresolved", "photo_id", photo.ID, "error", err)
			return
		}
		s.logger.Warn("scheduler: photo unresolved after the poll TTL",
			"photo_id", photo.ID,
			"request_id", photo.RequestID,
			"ttl", s.cfg.PhotoPollTTL.String(),
		)
		return
	}
	shift := photo.Attempts
	if shift > photoBackoffShift {
		shift = photoBackoffShift
	}
	next := now.Add(s.cfg.PhotoPollInterval * time.Duration(1<<shift))
	if limit := photo.TakenAt.Add(s.cfg.PhotoPollTTL); next.After(limit) {
		next = limit
	}
	if err := s.store.Photos.Defer(ctx, photo.ID, next, photo.Attempts+1); err != nil {
		s.logger.Error("scheduler: defer photo", "photo_id", photo.ID, "error", err)
	}
}

// commandPool bounds the number of concurrent outbound webcam commands to
// DISPATCH_MAX_CONCURRENT_COMMANDS. Callers take exactly one slot around one
// upstream interaction and never nest acquisitions.
type commandPool struct {
	slots chan struct{}
}

// newCommandPool builds a pool with size slots.
func newCommandPool(size int) *commandPool {
	if size < 1 {
		size = 1
	}
	return &commandPool{slots: make(chan struct{}, size)}
}

// withSlot runs fn while holding one slot, blocking while the pool is busy.
func (s *Scheduler) withSlot(ctx context.Context, fn func(context.Context) error) error {
	select {
	case s.slots.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.slots.slots }()
	return fn(ctx)
}

// dispatch runs every job concurrently and waits for all of them. Each job
// takes its own pool slot around its upstream call, so the number of in-flight
// outbound calls never exceeds the pool size.
func (s *Scheduler) dispatch(ctx context.Context, jobs []func(context.Context)) {
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job(ctx)
		}()
	}
	wg.Wait()
}
