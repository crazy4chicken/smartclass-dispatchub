package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/id"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/store"
)

// These tests need a real PostgreSQL server. Set DISPATCH_TEST_PG to a DSN and
// they create a throwaway schema per test, migrate it with the production
// migration runner and drop it again; without the variable every test skips.
const pgEnv = "DISPATCH_TEST_PG"

const (
	itTermCode = "2026-FALL"
	itRoomCode = "A301"
	itDeviceID = "dev-a301"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(pgEnv))
	if dsn == "" {
		t.Skipf("%s is not set; skipping PostgreSQL integration test", pgEnv)
	}

	admin, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to %s: %v", pgEnv, err)
	}
	schema := scratchSchema(t)
	if _, err := admin.Exec(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		_ = admin.Close(context.Background())
		t.Fatalf("create scratch schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Errorf("drop scratch schema %s: %v", schema, err)
		}
		_ = admin.Close(context.Background())
	})

	s, err := store.Open(context.Background(), withSearchPath(t, dsn, schema))
	if err != nil {
		t.Fatalf("open store on schema %s: %v", schema, err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate scratch schema %s: %v", schema, err)
	}
	return s
}

func scratchSchema(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("read random schema suffix: %v", err)
	}
	return "dispatch_test_" + hex.EncodeToString(raw)
}

// withSearchPath points the store pool at the scratch schema so the migrations
// and every query stay isolated from the database's public schema.
func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	if strings.Contains(dsn, "://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse %s: %v", pgEnv, err)
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	return dsn + " search_path=" + schema
}

func seedTermRoom(t *testing.T, s *store.Store) (domain.Term, domain.Room) {
	t.Helper()
	ctx := t.Context()
	term, err := s.Terms.Upsert(ctx, domain.Term{
		TermCode:    itTermCode,
		Name:        "2026 Fall",
		Week1Monday: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		Weeks:       16,
	})
	if err != nil {
		t.Fatalf("seed term: %v", err)
	}
	room, err := s.Rooms.Upsert(ctx, domain.Room{
		RoomCode:   itRoomCode,
		Name:       "A301",
		DeviceID:   itDeviceID,
		CameraEnum: 0,
		Enabled:    true,
	})
	if err != nil {
		t.Fatalf("seed room: %v", err)
	}
	return term, room
}

func addRoom(t *testing.T, s *store.Store, roomCode, deviceID string, enabled bool) domain.Room {
	t.Helper()
	room, err := s.Rooms.Upsert(t.Context(), domain.Room{
		RoomCode: roomCode, Name: roomCode, DeviceID: deviceID, CameraEnum: 0, Enabled: enabled,
	})
	if err != nil {
		t.Fatalf("upsert room %s: %v", roomCode, err)
	}
	return room
}

func manualSession(roomCode, status string, startsAt, endsAt time.Time) domain.Session {
	return domain.Session{
		ID:         id.New(),
		RoomCode:   roomCode,
		DeviceID:   itDeviceID,
		CameraEnum: 0,
		StartsAt:   startsAt,
		EndsAt:     endsAt,
		Status:     status,
		Origin:     domain.OriginManual,
	}
}

func createManual(t *testing.T, s *store.Store, roomCode, status string, startsAt, endsAt time.Time) domain.Session {
	t.Helper()
	stored, err := s.Sessions.CreateManual(t.Context(), manualSession(roomCode, status, startsAt, endsAt))
	if err != nil {
		t.Fatalf("create %s session in %s: %v", status, roomCode, err)
	}
	return stored
}

func entryFixture(importID, roomCode string) domain.Entry {
	return domain.Entry{
		ID:              id.New(),
		ImportID:        importID,
		TermCode:        itTermCode,
		CourseCode:      "CS101",
		CourseName:      "数据结构",
		TeacherUsername: "zhangsan",
		RoomCode:        roomCode,
		Weekday:         1,
		PeriodStart:     1,
		PeriodEnd:       2,
		Weeks:           "1-16",
	}
}

func importFixture(mode, sha string) domain.Import {
	return domain.Import{
		ID:         id.New(),
		TermCode:   itTermCode,
		Mode:       mode,
		Filename:   "timetable.csv",
		SHA256:     sha,
		RowCount:   1,
		OKCount:    1,
		Status:     domain.ImportCommitted,
		ImportedBy: "tester",
	}
}

func scheduledSession(entry domain.Entry, startsAt time.Time, status string) domain.Session {
	return domain.Session{
		ID:         id.New(),
		EntryID:    new(entry.ID),
		RoomCode:   entry.RoomCode,
		DeviceID:   itDeviceID,
		CameraEnum: 0,
		StartsAt:   startsAt,
		EndsAt:     startsAt.Add(45 * time.Minute),
		Status:     status,
		Origin:     domain.OriginSchedule,
	}
}

func commitImport(t *testing.T, s *store.Store, imp domain.Import, entries []domain.Entry, sessions []domain.Session) {
	t.Helper()
	if err := s.Imports.Commit(t.Context(), imp, entries, sessions); err != nil {
		t.Fatalf("commit import %s: %v", imp.ID, err)
	}
}

func getSession(t *testing.T, s *store.Store, sessionID string) domain.Session {
	t.Helper()
	sess, err := s.Sessions.Get(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("get session %s: %v", sessionID, err)
	}
	return sess
}

func sessionIDSet(sessions []domain.Session) map[string]bool {
	set := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		set[sess.ID] = true
	}
	return set
}

func requireNotFound(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("%s error = %v, want ErrNotFound", what, err)
	}
}

func requireConflict(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("%s error = %v, want ErrConflict", what, err)
	}
}

func TestIntegrationTermUpsertAndPeriods(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()

	first, err := s.Terms.Upsert(ctx, domain.Term{
		TermCode:    itTermCode,
		Name:        "2026 Fall",
		Week1Monday: time.Date(2026, 3, 2, 12, 30, 0, 0, time.UTC),
		Weeks:       16,
	})
	if err != nil {
		t.Fatalf("Upsert(term) error = %v", err)
	}
	if want := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC); !first.Week1Monday.Equal(want) {
		t.Fatalf("week1_monday = %s, want the calendar date at UTC midnight (%s)", first.Week1Monday, want)
	}
	if first.Name != "2026 Fall" || first.Weeks != 16 || first.CreatedAt.IsZero() {
		t.Fatalf("stored term = %+v", first)
	}

	got, err := s.Terms.Get(ctx, itTermCode)
	if err != nil {
		t.Fatalf("Get(term) error = %v", err)
	}
	if got.TermCode != first.TermCode || !got.Week1Monday.Equal(first.Week1Monday) {
		t.Fatalf("Get(term) = %+v, want %+v", got, first)
	}
	if _, err := s.Terms.Get(ctx, "2099-SPRING"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(missing term) error = %v, want ErrNotFound", err)
	}

	updated, err := s.Terms.Upsert(ctx, domain.Term{
		TermCode:    itTermCode,
		Name:        "Autumn 2026",
		Week1Monday: first.Week1Monday,
		Weeks:       18,
	})
	if err != nil {
		t.Fatalf("Upsert(existing term) error = %v", err)
	}
	if updated.Name != "Autumn 2026" || updated.Weeks != 18 {
		t.Fatalf("updated term = %+v, want the new name and week count", updated)
	}
	if !updated.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("created_at changed across upserts: %s -> %s", first.CreatedAt, updated.CreatedAt)
	}
	terms, err := s.Terms.List(ctx)
	if err != nil || len(terms) != 1 {
		t.Fatalf("List(terms) = %+v, err %v; want one term", terms, err)
	}

	periods := []domain.Period{
		{PeriodNo: 1, StartTime: "08:00", EndTime: "08:45"},
		{PeriodNo: 2, StartTime: "08:50", EndTime: "09:35"},
	}
	if err := s.Terms.ReplacePeriods(ctx, itTermCode, periods); err != nil {
		t.Fatalf("ReplacePeriods() error = %v", err)
	}
	stored, err := s.Terms.Periods(ctx, itTermCode)
	if err != nil || !slices.Equal(stored, periods) {
		t.Fatalf("Periods() = %+v, err %v; want %+v", stored, err, periods)
	}

	replacement := []domain.Period{{PeriodNo: 3, StartTime: "10:00", EndTime: "10:45"}}
	if err := s.Terms.ReplacePeriods(ctx, itTermCode, replacement); err != nil {
		t.Fatalf("ReplacePeriods(replacement) error = %v", err)
	}
	stored, err = s.Terms.Periods(ctx, itTermCode)
	if err != nil || !slices.Equal(stored, replacement) {
		t.Fatalf("Periods() after replacement = %+v, err %v; want only the new set", stored, err)
	}

	if err := s.Terms.ReplacePeriods(ctx, "2099-SPRING", periods); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ReplacePeriods(missing term) error = %v, want ErrNotFound", err)
	}
	if err := s.Terms.ReplacePeriods(ctx, itTermCode, []domain.Period{{PeriodNo: 5, StartTime: "12:00", EndTime: "11:00"}}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("ReplacePeriods(inverted period) error = %v, want ErrValidation", err)
	}
	if err := s.Terms.ReplacePeriods(ctx, itTermCode, []domain.Period{{PeriodNo: 21, StartTime: "08:00", EndTime: "09:00"}}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("ReplacePeriods(out-of-range period) error = %v, want ErrValidation", err)
	}
	stored, err = s.Terms.Periods(ctx, itTermCode)
	if err != nil || !slices.Equal(stored, replacement) {
		t.Fatalf("Periods() after rejected replacements = %+v, err %v; want the previous set intact", stored, err)
	}
}

func TestIntegrationImportCommitIsAtomic(t *testing.T) {
	s := testStore(t)
	seedTermRoom(t, s)
	ctx := t.Context()
	now := time.Now()

	good := importFixture(store.ModeAppend, "sha-good")
	entry := entryFixture(good.ID, itRoomCode)
	session := scheduledSession(entry, now.Add(time.Hour), domain.SessionPlanned)
	commitImport(t, s, good, []domain.Entry{entry}, []domain.Session{session})

	stored, err := s.Imports.Get(ctx, good.ID)
	if err != nil {
		t.Fatalf("Get(import) error = %v", err)
	}
	if stored.Status != domain.ImportCommitted || stored.Mode != store.ModeAppend || stored.RowCount != 1 || stored.ImportedBy != "tester" {
		t.Fatalf("stored import = %+v", stored)
	}
	if stored.Errors == nil {
		t.Fatalf("stored import errors = nil, want an empty list")
	}
	entries, err := s.Entries.ListByImport(ctx, good.ID)
	if err != nil || len(entries) != 1 || entries[0].ID != entry.ID {
		t.Fatalf("ListByImport() = %+v, err %v; want the committed entry", entries, err)
	}
	got := getSession(t, s, session.ID)
	if got.Status != domain.SessionPlanned || got.Origin != domain.OriginSchedule ||
		got.EntryID == nil || *got.EntryID != entry.ID || got.RoomCode != itRoomCode || got.DeviceID != itDeviceID {
		t.Fatalf("committed session = %+v", got)
	}

	// A failure half way through the batch (duplicate session primary key)
	// must roll back the batch row and every entry/session already written.
	bad := importFixture(store.ModeAppend, "sha-bad")
	badEntry := entryFixture(bad.ID, itRoomCode)
	duplicate := scheduledSession(badEntry, now.Add(2*time.Hour), domain.SessionPlanned)
	err = s.Imports.Commit(ctx, bad, []domain.Entry{badEntry}, []domain.Session{duplicate, duplicate})
	if err == nil {
		t.Fatalf("Commit(duplicate session id) error = nil, want a failure")
	}
	if _, err := s.Imports.Get(ctx, bad.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(failed import) error = %v, want ErrNotFound", err)
	}
	if entries, err := s.Entries.ListByImport(ctx, bad.ID); err != nil || len(entries) != 0 {
		t.Fatalf("ListByImport(failed) = %+v, err %v; want no entries", entries, err)
	}
	if _, err := s.Sessions.Get(ctx, duplicate.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(rolled back session) error = %v, want ErrNotFound", err)
	}

	// Validation failures are refused before any write.
	invalid := importFixture(store.ModeAppend, "sha-invalid")
	invalidEntry := entryFixture(invalid.ID, itRoomCode)
	invalidSession := scheduledSession(invalidEntry, now.Add(3*time.Hour), domain.SessionPlanned)
	invalidSession.DeviceID = ""
	if err := s.Imports.Commit(ctx, invalid, []domain.Entry{invalidEntry}, []domain.Session{invalidSession}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("Commit(session without a device) error = %v, want ErrValidation", err)
	}
	if _, err := s.Imports.Get(ctx, invalid.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(validation-failed import) error = %v, want ErrNotFound", err)
	}
	noEntryID := importFixture(store.ModeAppend, "sha-no-entry-id")
	if err := s.Imports.Commit(ctx, noEntryID, []domain.Entry{{}}, nil); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("Commit(entry without an id) error = %v, want ErrValidation", err)
	}

	// The failed batches left the good one untouched.
	entries, err = s.Entries.ListByImport(ctx, good.ID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListByImport(good) = %+v, err %v; want the batch intact", entries, err)
	}
	if found, err := s.Imports.FindBySHA(ctx, itTermCode, "sha-good"); err != nil || found.ID != good.ID {
		t.Fatalf("FindBySHA(committed) = %+v, err %v", found, err)
	}
	if _, err := s.Imports.FindBySHA(ctx, itTermCode, "sha-bad"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("FindBySHA(failed import) error = %v, want ErrNotFound", err)
	}
}

// TestIntegrationReplaceSupersedesAndCancelsPlanned covers the replace-import
// cutover: previous entries are superseded, only their future planned sessions
// are canceled and live/completed recordings are never touched.
func TestIntegrationReplaceSupersedesAndCancelsPlanned(t *testing.T) {
	s := testStore(t)
	seedTermRoom(t, s)
	ctx := t.Context()
	now := time.Now()

	first := importFixture(store.ModeAppend, "sha-first")
	entryFirst := entryFixture(first.ID, itRoomCode)
	sessions := []domain.Session{
		scheduledSession(entryFirst, now.Add(2*time.Hour), domain.SessionPlanned),
		scheduledSession(entryFirst, now.Add(-3*time.Hour), domain.SessionPlanned),
		scheduledSession(entryFirst, now.Add(-30*time.Minute), domain.SessionRecording),
		scheduledSession(entryFirst, now.Add(-5*time.Hour), domain.SessionCompleted),
	}
	commitImport(t, s, first, []domain.Entry{entryFirst}, sessions)

	second := importFixture(store.ModeReplace, "sha-second")
	entrySecond := entryFixture(second.ID, itRoomCode)
	secondSession := scheduledSession(entrySecond, now.Add(3*time.Hour), domain.SessionPlanned)
	commitImport(t, s, second, []domain.Entry{entrySecond}, []domain.Session{secondSession})

	live, err := s.Entries.ListByTerm(ctx, itTermCode)
	if err != nil || len(live) != 1 || live[0].ID != entrySecond.ID {
		t.Fatalf("ListByTerm() = %+v, err %v; want only the new entry to be live", live, err)
	}
	history, err := s.Entries.ListByImport(ctx, first.ID)
	if err != nil || len(history) != 1 || history[0].ID != entryFirst.ID {
		t.Fatalf("ListByImport(first) = %+v, err %v; want the superseded entry kept for the record", history, err)
	}

	wantStatuses := map[string]string{
		sessions[0].ID:   domain.SessionCanceled,  // future planned -> canceled
		sessions[1].ID:   domain.SessionPlanned,   // already started -> kept
		sessions[2].ID:   domain.SessionRecording, // live -> never touched
		sessions[3].ID:   domain.SessionCompleted, // terminal -> never touched
		secondSession.ID: domain.SessionPlanned,
	}
	for sessionID, want := range wantStatuses {
		if got := getSession(t, s, sessionID); got.Status != want {
			t.Fatalf("session %s status = %q, want %q", sessionID, got.Status, want)
		}
	}

	for _, sha := range []string{"sha-first", "sha-second"} {
		if _, err := s.Imports.FindBySHA(ctx, itTermCode, sha); err != nil {
			t.Fatalf("FindBySHA(%s) error = %v", sha, err)
		}
	}

	// A third replace supersedes the second batch too, canceling its future
	// planned session.
	third := importFixture(store.ModeReplace, "sha-third")
	entryThird := entryFixture(third.ID, itRoomCode)
	commitImport(t, s, third, []domain.Entry{entryThird}, nil)
	if got := getSession(t, s, secondSession.ID); got.Status != domain.SessionCanceled {
		t.Fatalf("session from the superseded batch status = %q, want canceled", got.Status)
	}
	live, err = s.Entries.ListByTerm(ctx, itTermCode)
	if err != nil || len(live) != 1 || live[0].ID != entryThird.ID {
		t.Fatalf("ListByTerm() after the third import = %+v, err %v", live, err)
	}
}

func TestIntegrationClaimDueIsClaimOnce(t *testing.T) {
	s := testStore(t)
	seedTermRoom(t, s)
	ctx := t.Context()
	now := time.Now()
	base := now.Add(time.Minute)

	created := make([]domain.Session, 0, 12)
	for i := range 12 {
		startsAt := base.Add(time.Duration(i) * time.Second)
		created = append(created, createManual(t, s, itRoomCode, domain.SessionPlanned, startsAt, startsAt.Add(30*time.Minute)))
	}
	// An occurrence that already ended is never claimed.
	ended := createManual(t, s, itRoomCode, domain.SessionPlanned, now.Add(-2*time.Hour), now.Add(-time.Hour))

	first, err := s.Sessions.ClaimDue(ctx, now, 2*time.Minute, 5)
	if err != nil {
		t.Fatalf("ClaimDue(first) error = %v", err)
	}
	second, err := s.Sessions.ClaimDue(ctx, now, 2*time.Minute, 5)
	if err != nil {
		t.Fatalf("ClaimDue(second) error = %v", err)
	}
	third, err := s.Sessions.ClaimDue(ctx, now, 2*time.Minute, 5)
	if err != nil {
		t.Fatalf("ClaimDue(third) error = %v", err)
	}
	fourth, err := s.Sessions.ClaimDue(ctx, now, 2*time.Minute, 5)
	if err != nil {
		t.Fatalf("ClaimDue(fourth) error = %v", err)
	}
	if len(first) != 5 || len(second) != 5 || len(third) != 2 || len(fourth) != 0 {
		t.Fatalf("claim batch sizes = %d/%d/%d/%d, want 5/5/2/0", len(first), len(second), len(third), len(fourth))
	}

	claimed := sessionIDSet(append(append(append(first, second...), third...), fourth...))
	if claimed[ended.ID] {
		t.Fatalf("an already-ended occurrence was claimed")
	}
	if len(claimed) != 12 {
		t.Fatalf("claimed %d distinct sessions, want 12 (each row exactly once)", len(claimed))
	}
	for _, sess := range created {
		if !claimed[sess.ID] {
			t.Fatalf("session %s was never claimed", sess.ID)
		}
	}
	for _, sess := range first {
		if sess.Status != domain.SessionStarting {
			t.Fatalf("claimed session status = %q, want starting", sess.Status)
		}
	}
	if got := getSession(t, s, first[0].ID); got.Status != domain.SessionStarting {
		t.Fatalf("persisted status = %q, want starting", got.Status)
	}
	if got := getSession(t, s, ended.ID); got.Status != domain.SessionPlanned {
		t.Fatalf("ended occurrence status = %q, want it left planned", got.Status)
	}
}

// TestIntegrationClaimDueConcurrentClaimsAreDisjoint proves the SKIP LOCKED
// claim: two concurrent schedulers never receive the same row.
func TestIntegrationClaimDueConcurrentClaimsAreDisjoint(t *testing.T) {
	s := testStore(t)
	seedTermRoom(t, s)
	ctx := t.Context()
	now := time.Now()
	base := now.Add(time.Minute)

	for i := range 12 {
		startsAt := base.Add(time.Duration(i) * time.Second)
		createManual(t, s, itRoomCode, domain.SessionPlanned, startsAt, startsAt.Add(30*time.Minute))
	}

	results := make([][]domain.Session, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index], errs[index] = s.Sessions.ClaimDue(ctx, now, 2*time.Minute, 6)
		}(i)
	}
	close(start)
	wait.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent ClaimDue %d error = %v", i, err)
		}
	}
	if len(results[0]) != 6 || len(results[1]) != 6 {
		t.Fatalf("concurrent claim sizes = %d/%d, want 6/6", len(results[0]), len(results[1]))
	}
	seen := map[string]bool{}
	for _, batch := range results {
		for _, sess := range batch {
			if seen[sess.ID] {
				t.Fatalf("session %s was claimed by both callers", sess.ID)
			}
			seen[sess.ID] = true
		}
	}
	if len(seen) != 12 {
		t.Fatalf("both callers claimed %d distinct rows in total, want 12", len(seen))
	}
}

// TestIntegrationClaimDueSkipsLockedRows pins the FOR UPDATE SKIP LOCKED
// behaviour directly: a row locked by another transaction is skipped and stays
// claimable after the lock is released.
func TestIntegrationClaimDueSkipsLockedRows(t *testing.T) {
	s := testStore(t)
	seedTermRoom(t, s)
	ctx := t.Context()
	now := time.Now()

	var created []domain.Session
	for i := range 3 {
		startsAt := now.Add(time.Minute + time.Duration(i)*time.Second)
		created = append(created, createManual(t, s, itRoomCode, domain.SessionPlanned, startsAt, startsAt.Add(30*time.Minute)))
	}
	lockedID := created[0].ID

	locked := make(chan struct{})
	release := make(chan struct{})
	txResult := make(chan error, 1)
	go func() {
		txResult <- s.Tx(ctx, func(tx pgx.Tx) error {
			var got string
			if err := tx.QueryRow(ctx, `SELECT id FROM sessions WHERE id = $1 FOR UPDATE`, lockedID).Scan(&got); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()

	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("locking transaction did not acquire the row lock")
	}

	claimed, err := s.Sessions.ClaimDue(ctx, now, 2*time.Minute, 3)
	if err != nil {
		t.Fatalf("ClaimDue(while locked) error = %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("ClaimDue(while locked) returned %d rows, want the 2 unlocked rows", len(claimed))
	}
	for _, sess := range claimed {
		if sess.ID == lockedID {
			t.Fatalf("ClaimDue returned the row locked by another transaction")
		}
	}

	close(release)
	if err := <-txResult; err != nil {
		t.Fatalf("locking transaction error = %v", err)
	}

	after, err := s.Sessions.ClaimDue(ctx, now, 2*time.Minute, 3)
	if err != nil {
		t.Fatalf("ClaimDue(after release) error = %v", err)
	}
	if len(after) != 1 || after[0].ID != lockedID {
		t.Fatalf("ClaimDue(after release) = %+v, want the previously locked row", after)
	}
}

func TestIntegrationClaimExpired(t *testing.T) {
	s := testStore(t)
	seedTermRoom(t, s)
	ctx := t.Context()
	now := time.Now()

	ended1 := createManual(t, s, itRoomCode, domain.SessionRecording, now.Add(-2*time.Hour), now.Add(-5*time.Minute))
	ended2 := createManual(t, s, itRoomCode, domain.SessionRecording, now.Add(-2*time.Hour), now.Add(-10*time.Second))
	future := createManual(t, s, itRoomCode, domain.SessionRecording, now, now.Add(time.Hour))

	claimed, err := s.Sessions.ClaimExpired(ctx, now, 0, 10)
	if err != nil {
		t.Fatalf("ClaimExpired() error = %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("ClaimExpired() returned %d sessions, want 2", len(claimed))
	}
	set := sessionIDSet(claimed)
	if !set[ended1.ID] || !set[ended2.ID] || set[future.ID] {
		t.Fatalf("ClaimExpired() = %+v, want exactly the two ended recordings", sessionIDsOf(claimed))
	}
	for _, sess := range claimed {
		if sess.Status != domain.SessionStopping {
			t.Fatalf("claimed recording status = %q, want stopping", sess.Status)
		}
	}
	again, err := s.Sessions.ClaimExpired(ctx, now, 0, 10)
	if err != nil || len(again) != 0 {
		t.Fatalf("ClaimExpired(second) = %+v, err %v; want no rows", again, err)
	}
	if got := getSession(t, s, future.ID); got.Status != domain.SessionRecording {
		t.Fatalf("unexpired recording status = %q, want recording", got.Status)
	}

	// The grace defers the claim: a recording that ended one minute ago is
	// only claimable once now - grace passes its end.
	recent := createManual(t, s, itRoomCode, domain.SessionRecording, now.Add(-90*time.Minute), now.Add(-time.Minute))
	grace, err := s.Sessions.ClaimExpired(ctx, now, 2*time.Minute, 10)
	if err != nil || len(grace) != 0 {
		t.Fatalf("ClaimExpired(within grace) = %d rows, err %v; want none", len(grace), err)
	}
	later, err := s.Sessions.ClaimExpired(ctx, now.Add(2*time.Minute), 0, 10)
	if err != nil || len(later) != 1 || later[0].ID != recent.ID {
		t.Fatalf("ClaimExpired(after grace) = %+v, err %v; want the recent recording", sessionIDsOf(later), err)
	}
}

func sessionIDsOf(sessions []domain.Session) []string {
	ids := make([]string, len(sessions))
	for i, sess := range sessions {
		ids[i] = sess.ID
	}
	return ids
}

// TestIntegrationSessionLifecycle walks the state machine through the store:
// every guard is exercised from both sides.
func TestIntegrationSessionLifecycle(t *testing.T) {
	s := testStore(t)
	seedTermRoom(t, s)
	ctx := t.Context()
	now := time.Now()

	// planned -> recording -> completed
	recording := createManual(t, s, itRoomCode, domain.SessionPlanned, now.Add(time.Hour), now.Add(2*time.Hour))
	if err := s.Sessions.MarkRecording(ctx, recording.ID, "stream-1", now); err != nil {
		t.Fatalf("MarkRecording() error = %v", err)
	}
	got := getSession(t, s, recording.ID)
	if got.Status != domain.SessionRecording || got.StreamID == nil || *got.StreamID != "stream-1" ||
		got.StartedAt == nil || got.LastError != nil {
		t.Fatalf("session after MarkRecording = %+v", got)
	}
	requireConflict(t, s.Sessions.MarkRecording(ctx, recording.ID, "stream-2", now), "MarkRecording(recording)")

	if err := s.Sessions.MarkCompleted(ctx, recording.ID, now.Add(45*time.Minute)); err != nil {
		t.Fatalf("MarkCompleted() error = %v", err)
	}
	got = getSession(t, s, recording.ID)
	if got.Status != domain.SessionCompleted || got.StoppedAt == nil {
		t.Fatalf("session after MarkCompleted = %+v", got)
	}
	if err := s.Sessions.MarkCompleted(ctx, recording.ID, now.Add(46*time.Minute)); err != nil {
		t.Fatalf("MarkCompleted(replay) error = %v, want it idempotent", err)
	}
	requireConflict(t, s.Sessions.MarkStopped(ctx, recording.ID), "MarkStopped(completed)")
	requireConflict(t, s.Sessions.MarkFailed(ctx, recording.ID, "late"), "MarkFailed(completed)")

	// failed is terminal but repeatable
	failed := createManual(t, s, itRoomCode, domain.SessionPlanned, now.Add(2*time.Hour), now.Add(3*time.Hour))
	if err := s.Sessions.MarkFailed(ctx, failed.ID, "device_offline"); err != nil {
		t.Fatalf("MarkFailed() error = %v", err)
	}
	got = getSession(t, s, failed.ID)
	if got.Status != domain.SessionFailed || got.LastError == nil || *got.LastError != "device_offline" {
		t.Fatalf("session after MarkFailed = %+v", got)
	}
	if err := s.Sessions.MarkFailed(ctx, failed.ID, "device_offline"); err != nil {
		t.Fatalf("MarkFailed(replay) error = %v, want it allowed", err)
	}

	// retries accumulate
	retrying := createManual(t, s, itRoomCode, domain.SessionPlanned, now.Add(3*time.Hour), now.Add(4*time.Hour))
	for want := 1; want <= 2; want++ {
		count, err := s.Sessions.BumpRetry(ctx, retrying.ID, "upstream_unavailable")
		if err != nil || count != want {
			t.Fatalf("BumpRetry() = %d, err %v; want %d", count, err, want)
		}
	}
	got = getSession(t, s, retrying.ID)
	if got.RetryCount != 2 || got.LastError == nil || got.Status != domain.SessionPlanned {
		t.Fatalf("session after retries = %+v", got)
	}

	// stream ids are unique across every session that carries one
	requireNotFound(t, s.Sessions.SetStreamID(ctx, "missing-session", "stream-x"), "SetStreamID(missing session)")
	requireConflict(t, s.Sessions.SetStreamID(ctx, retrying.ID, "stream-1"), "SetStreamID(duplicate stream)")
	if err := s.Sessions.SetStreamID(ctx, retrying.ID, "stream-3"); err != nil {
		t.Fatalf("SetStreamID() error = %v", err)
	}
	if got := getSession(t, s, retrying.ID); got.StreamID == nil || *got.StreamID != "stream-3" {
		t.Fatalf("session stream_id = %v, want stream-3", got.StreamID)
	}

	// missed housekeeping only touches planned sessions that already started
	missed := createManual(t, s, itRoomCode, domain.SessionPlanned, now.Add(-time.Hour), now.Add(-15*time.Minute))
	if count, err := s.Sessions.MarkMissed(ctx, now); err != nil || count != 1 {
		t.Fatalf("MarkMissed() = %d, err %v; want 1", count, err)
	}
	if got := getSession(t, s, missed.ID); got.Status != domain.SessionMissed {
		t.Fatalf("missed session status = %q, want missed", got.Status)
	}
	if count, err := s.Sessions.MarkMissed(ctx, now); err != nil || count != 0 {
		t.Fatalf("MarkMissed(second) = %d, err %v; want 0", count, err)
	}

	// A failed or missed session can be started manually: the MarkRecording
	// guard accepts starting/planned/failed/missed.
	for _, status := range []string{domain.SessionFailed, domain.SessionMissed} {
		revived := createManual(t, s, itRoomCode, status, now.Add(5*time.Hour), now.Add(6*time.Hour))
		if err := s.Sessions.MarkRecording(ctx, revived.ID, "stream-"+status, now); err != nil {
			t.Fatalf("MarkRecording(%s) error = %v, want a manual re-start to be allowed", status, err)
		}
		if got := getSession(t, s, revived.ID); got.Status != domain.SessionRecording {
			t.Fatalf("re-started %s session status = %q, want recording", status, got.Status)
		}
	}

	// full cycle: planned -> starting -> recording -> stopping -> recording -> completed
	cycle := createManual(t, s, itRoomCode, domain.SessionPlanned, now.Add(time.Minute), now.Add(30*time.Minute))
	due, err := s.Sessions.ClaimDue(ctx, now, 2*time.Minute, 10)
	if err != nil || len(due) != 1 || due[0].ID != cycle.ID {
		t.Fatalf("ClaimDue() = %+v, err %v; want only the due cycle session", sessionIDsOf(due), err)
	}
	if err := s.Sessions.MarkRecording(ctx, cycle.ID, "stream-cycle", now); err != nil {
		t.Fatalf("MarkRecording(cycle) error = %v", err)
	}
	expired, err := s.Sessions.ClaimExpired(ctx, now.Add(time.Hour), 0, 10)
	if err != nil || len(expired) != 1 || expired[0].ID != cycle.ID {
		t.Fatalf("ClaimExpired() = %+v, err %v; want the cycle session", sessionIDsOf(expired), err)
	}
	if err := s.Sessions.MarkStopped(ctx, cycle.ID); err != nil {
		t.Fatalf("MarkStopped() error = %v, want the stop-back transition", err)
	}
	if got := getSession(t, s, cycle.ID); got.Status != domain.SessionRecording {
		t.Fatalf("session after MarkStopped = %q, want recording", got.Status)
	}
	if err := s.Sessions.MarkCompleted(ctx, cycle.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("MarkCompleted(cycle) error = %v", err)
	}
	if got := getSession(t, s, cycle.ID); got.Status != domain.SessionCompleted {
		t.Fatalf("cycle session status = %q, want completed", got.Status)
	}
}

func TestIntegrationSessionQueries(t *testing.T) {
	s := testStore(t)
	seedTermRoom(t, s)
	ctx := t.Context()
	now := time.Now()

	requireNotFound(t, func() error {
		_, err := s.Sessions.ActiveByRoom(ctx, itRoomCode)
		return err
	}(), "ActiveByRoom(no live session)")

	live := createManual(t, s, itRoomCode, domain.SessionRecording, now.Add(-10*time.Minute), now.Add(30*time.Minute))
	active, err := s.Sessions.ActiveByRoom(ctx, itRoomCode)
	if err != nil || active.ID != live.ID {
		t.Fatalf("ActiveByRoom() = %+v, err %v; want the live session", active, err)
	}

	addRoom(t, s, "B102", "dev-b102", true)
	older := createManual(t, s, "B102", domain.SessionPlanned, now.Add(time.Hour), now.Add(2*time.Hour))
	newer := createManual(t, s, "B102", domain.SessionPlanned, now.Add(2*time.Hour), now.Add(3*time.Hour))

	listed, err := s.Sessions.List(ctx, store.SessionFilter{RoomCode: "B102"})
	if err != nil || len(listed) != 2 || listed[0].ID != newer.ID || listed[1].ID != older.ID {
		t.Fatalf("List(room=B102) = %+v, err %v; want newest starts_at first", sessionIDsOf(listed), err)
	}
	recordings, err := s.Sessions.List(ctx, store.SessionFilter{Status: domain.SessionRecording})
	if err != nil || len(recordings) != 1 || recordings[0].ID != live.ID {
		t.Fatalf("List(status=recording) = %+v, err %v", sessionIDsOf(recordings), err)
	}
	if _, err := s.Sessions.List(ctx, store.SessionFilter{Date: "not-a-date"}); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("List(invalid date) error = %v, want ErrValidation", err)
	}

	// Room guards: a room with non-terminal future sessions cannot be unbound.
	has, err := s.Rooms.HasSessionsAfter(ctx, itRoomCode, now)
	if err != nil || !has {
		t.Fatalf("HasSessionsAfter(A301) = %v, err %v; want true for the live recording", has, err)
	}
	addRoom(t, s, "D404", "dev-d404", true)
	has, err = s.Rooms.HasSessionsAfter(ctx, "D404", now)
	if err != nil || has {
		t.Fatalf("HasSessionsAfter(D404) = %v, err %v; want false", has, err)
	}
	requireConflict(t, s.Rooms.Delete(ctx, itRoomCode), "Delete(referenced room)")
	requireNotFound(t, s.Rooms.Delete(ctx, "Z999"), "Delete(unknown room)")
	if err := s.Rooms.Delete(ctx, "D404"); err != nil {
		t.Fatalf("Delete(unused room) error = %v", err)
	}
	requireNotFound(t, func() error {
		_, err := s.Rooms.Get(ctx, "D404")
		return err
	}(), "Get(deleted room)")

	room, err := s.Rooms.GetByDevice(ctx, itDeviceID, 0)
	if err != nil || room.RoomCode != itRoomCode {
		t.Fatalf("GetByDevice() = %+v, err %v", room, err)
	}
	addRoom(t, s, "E505", "dev-e505", false)
	enabled, err := s.Rooms.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("ListEnabled() error = %v", err)
	}
	for _, candidate := range enabled {
		if candidate.RoomCode == "E505" {
			t.Fatalf("ListEnabled() returned a disabled room: %+v", candidate)
		}
	}
}

func TestIntegrationCancelFutureAndMissed(t *testing.T) {
	s := testStore(t)
	seedTermRoom(t, s)
	ctx := t.Context()
	now := time.Now()

	addRoom(t, s, "B102", "dev-b102", true)
	future := createManual(t, s, "B102", domain.SessionPlanned, now.Add(2*time.Hour), now.Add(3*time.Hour))
	past := createManual(t, s, "B102", domain.SessionPlanned, now.Add(-2*time.Hour), now.Add(-time.Hour))
	live := createManual(t, s, "B102", domain.SessionRecording, now.Add(-10*time.Minute), now.Add(30*time.Minute))
	done := createManual(t, s, "B102", domain.SessionCompleted, now.Add(-3*time.Hour), now.Add(-2*time.Hour))

	count, err := s.Sessions.CancelFuture(ctx, "B102")
	if err != nil || count != 1 {
		t.Fatalf("CancelFuture() = %d, err %v; want 1", count, err)
	}
	if got := getSession(t, s, future.ID); got.Status != domain.SessionCanceled {
		t.Fatalf("future planned status = %q, want canceled", got.Status)
	}
	if got := getSession(t, s, past.ID); got.Status != domain.SessionPlanned {
		t.Fatalf("already-started planned status = %q, want it left planned", got.Status)
	}
	if got := getSession(t, s, live.ID); got.Status != domain.SessionRecording {
		t.Fatalf("live recording status = %q, want it untouched", got.Status)
	}
	if got := getSession(t, s, done.ID); got.Status != domain.SessionCompleted {
		t.Fatalf("completed session status = %q, want it untouched", got.Status)
	}
	if count, err := s.Sessions.CancelFuture(ctx, "B102"); err != nil || count != 0 {
		t.Fatalf("CancelFuture(second) = %d, err %v; want 0", count, err)
	}
	// Another room is not affected.
	other := createManual(t, s, itRoomCode, domain.SessionPlanned, now.Add(2*time.Hour), now.Add(3*time.Hour))
	if count, err := s.Sessions.CancelFuture(ctx, itRoomCode); err != nil || count != 1 {
		t.Fatalf("CancelFuture(A301) = %d, err %v; want 1", count, err)
	}
	if got := getSession(t, s, other.ID); got.Status != domain.SessionCanceled {
		t.Fatalf("other room future session status = %q, want canceled", got.Status)
	}

	// MarkMissed only turns planned sessions that already started into missed.
	count, err = s.Sessions.MarkMissed(ctx, now)
	if err != nil || count != 1 {
		t.Fatalf("MarkMissed() = %d, err %v; want the one past planned session", count, err)
	}
	if got := getSession(t, s, past.ID); got.Status != domain.SessionMissed {
		t.Fatalf("past planned status = %q, want missed", got.Status)
	}
	if got := getSession(t, s, live.ID); got.Status != domain.SessionRecording {
		t.Fatalf("recording was marked missed")
	}
}

func photoFixture(sessionID *string, requestID string, nextPollAt *time.Time) domain.Photo {
	return domain.Photo{
		ID:         id.New(),
		SessionID:  sessionID,
		RoomCode:   itRoomCode,
		DeviceID:   itDeviceID,
		CameraEnum: 0,
		RequestID:  requestID,
		Source:     "manual",
		ActorID:    new("user-1"),
		Status:     domain.PhotoPending,
		NextPollAt: nextPollAt,
	}
}

func TestIntegrationPhotosClaimResolve(t *testing.T) {
	s := testStore(t)
	seedTermRoom(t, s)
	ctx := t.Context()
	now := time.Now()

	session := createManual(t, s, itRoomCode, domain.SessionPlanned, now.Add(time.Hour), now.Add(2*time.Hour))
	create := func(sessionID *string, requestID string, nextPollAt *time.Time) domain.Photo {
		t.Helper()
		stored, err := s.Photos.Create(ctx, photoFixture(sessionID, requestID, nextPollAt))
		if err != nil {
			t.Fatalf("Create(photo %s) error = %v", requestID, err)
		}
		return stored
	}
	due1 := create(nil, "req-1", nil)
	due2 := create(nil, "req-2", nil)
	futurePoll := create(nil, "req-3", new(now.Add(time.Hour)))
	linked := create(new(session.ID), "req-4", nil)

	claimed, err := s.Photos.ClaimPending(ctx, now, 10)
	if err != nil {
		t.Fatalf("ClaimPending() error = %v", err)
	}
	if len(claimed) != 3 {
		t.Fatalf("ClaimPending() returned %d photos, want the 3 due ones", len(claimed))
	}
	set := map[string]bool{}
	for _, photo := range claimed {
		set[photo.ID] = true
		if photo.NextPollAt == nil || !photo.NextPollAt.After(now) {
			t.Fatalf("claimed photo %s next_poll_at = %v, want a lease in the future", photo.ID, photo.NextPollAt)
		}
	}
	if !set[due1.ID] || !set[due2.ID] || !set[linked.ID] || set[futurePoll.ID] {
		t.Fatalf("ClaimPending() = %+v, want the three due photos", claimed)
	}
	again, err := s.Photos.ClaimPending(ctx, now, 10)
	if err != nil || len(again) != 0 {
		t.Fatalf("ClaimPending(second) = %d photos, err %v; want none while leased", len(again), err)
	}

	if err := s.Photos.Resolve(ctx, due1.ID, "upstream-photo-1", now); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	resolved, err := s.Photos.Get(ctx, due1.ID)
	if err != nil {
		t.Fatalf("Get(resolved photo) error = %v", err)
	}
	if resolved.Status != domain.PhotoResolved || resolved.PhotoID == nil || *resolved.PhotoID != "upstream-photo-1" ||
		resolved.ResolvedAt == nil || resolved.NextPollAt != nil {
		t.Fatalf("resolved photo = %+v", resolved)
	}
	requireConflict(t, s.Photos.Resolve(ctx, "missing-photo", "upstream-photo-x", now), "Resolve(missing photo)")
	requireConflict(t, s.Photos.Resolve(ctx, due2.ID, "upstream-photo-1", now), "Resolve(duplicate upstream photo id)")

	if err := s.Photos.Defer(ctx, futurePoll.ID, now.Add(time.Minute), 4); err != nil {
		t.Fatalf("Defer() error = %v", err)
	}
	deferred, err := s.Photos.Get(ctx, futurePoll.ID)
	if err != nil || deferred.Attempts != 4 || deferred.Status != domain.PhotoPending ||
		deferred.NextPollAt == nil || !deferred.NextPollAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("deferred photo = %+v, err %v", deferred, err)
	}
	claimedLater, err := s.Photos.ClaimPending(ctx, now.Add(2*time.Minute), 10)
	if err != nil || len(claimedLater) != 1 || claimedLater[0].ID != futurePoll.ID {
		t.Fatalf("ClaimPending(after defer) = %+v, err %v; want only the deferred photo", claimedLater, err)
	}

	if err := s.Photos.MarkUnresolved(ctx, futurePoll.ID); err != nil {
		t.Fatalf("MarkUnresolved() error = %v", err)
	}
	unresolved, err := s.Photos.Get(ctx, futurePoll.ID)
	if err != nil || unresolved.Status != domain.PhotoUnresolved || unresolved.NextPollAt != nil {
		t.Fatalf("unresolved photo = %+v, err %v", unresolved, err)
	}
	final, err := s.Photos.ClaimPending(ctx, now.Add(time.Hour), 10)
	if err != nil || len(final) != 0 {
		t.Fatalf("ClaimPending(after resolution) = %d photos, err %v; want none", len(final), err)
	}

	if byRequest, err := s.Photos.ByRequestID(ctx, "req-3"); err != nil || byRequest.ID != futurePoll.ID {
		t.Fatalf("ByRequestID() = %+v, err %v", byRequest, err)
	}
	requireNotFound(t, func() error {
		_, err := s.Photos.ByRequestID(ctx, "missing-request")
		return err
	}(), "ByRequestID(unknown)")
	requireNotFound(t, func() error {
		_, err := s.Photos.Get(ctx, "missing-photo")
		return err
	}(), "Get(missing photo)")

	linkedPhotos, err := s.Photos.ListBySession(ctx, session.ID)
	if err != nil || len(linkedPhotos) != 1 || linkedPhotos[0].ID != linked.ID {
		t.Fatalf("ListBySession() = %+v, err %v", linkedPhotos, err)
	}
}

func TestIntegrationPhotosConcurrentClaimsAreDisjoint(t *testing.T) {
	s := testStore(t)
	seedTermRoom(t, s)
	ctx := t.Context()
	now := time.Now()

	for i := range 10 {
		if _, err := s.Photos.Create(ctx, photoFixture(nil, "req-"+string(rune('a'+i)), nil)); err != nil {
			t.Fatalf("Create(photo %d) error = %v", i, err)
		}
	}

	results := make([][]domain.Photo, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index], errs[index] = s.Photos.ClaimPending(ctx, now, 4)
		}(i)
	}
	close(start)
	wait.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent ClaimPending %d error = %v", i, err)
		}
	}
	if len(results[0]) != 4 || len(results[1]) != 4 {
		t.Fatalf("concurrent claim sizes = %d/%d, want 4/4", len(results[0]), len(results[1]))
	}
	seen := map[string]bool{}
	for _, batch := range results {
		for _, photo := range batch {
			if seen[photo.ID] {
				t.Fatalf("photo %s was claimed by both callers", photo.ID)
			}
			seen[photo.ID] = true
		}
	}
	rest, err := s.Photos.ClaimPending(ctx, now, 10)
	if err != nil || len(rest) != 2 {
		t.Fatalf("ClaimPending(rest) = %d photos, err %v; want the remaining 2", len(rest), err)
	}
}

func TestIntegrationCommandsIdempotency(t *testing.T) {
	s := testStore(t)
	seedTermRoom(t, s)
	ctx := t.Context()
	now := time.Now()

	session := createManual(t, s, itRoomCode, domain.SessionPlanned, now.Add(time.Hour), now.Add(2*time.Hour))
	key := "idem-key-1"
	audit := domain.Command{
		Action:         domain.ActionStart,
		RoomCode:       itRoomCode,
		DeviceID:       itDeviceID,
		CameraEnum:     0,
		ActorID:        "user-1",
		IdempotencyKey: new(key),
		Outcome:        domain.OutcomeAccepted,
		HTTPStatus:     new(201),
		Detail:         new("stream created"),
	}
	stored, err := s.Commands.Insert(ctx, audit)
	if err != nil {
		t.Fatalf("Insert(command) error = %v", err)
	}
	if stored.ID == "" || stored.CreatedAt.IsZero() || stored.Outcome != domain.OutcomeAccepted {
		t.Fatalf("stored command = %+v", stored)
	}
	if stored.HTTPStatus == nil || *stored.HTTPStatus != 201 || stored.Detail == nil || *stored.Detail != "stream created" {
		t.Fatalf("stored command audit fields = %+v", stored)
	}

	replay, err := s.Commands.ByIdempotencyKey(ctx, key)
	if err != nil {
		t.Fatalf("ByIdempotencyKey() error = %v", err)
	}
	if replay.ID != stored.ID || replay.Outcome != stored.Outcome ||
		replay.HTTPStatus == nil || *replay.HTTPStatus != 201 || replay.CreatedAt.IsZero() {
		t.Fatalf("replayed command = %+v, want the stored row", replay)
	}
	requireConflict(t, func() error {
		_, err := s.Commands.Insert(ctx, audit)
		return err
	}(), "Insert(duplicate idempotency key)")
	requireNotFound(t, func() error {
		_, err := s.Commands.ByIdempotencyKey(ctx, "never-used")
		return err
	}(), "ByIdempotencyKey(unknown)")

	// An empty key is stored as NULL, so it never collides and is not
	// findable by lookup.
	for i := range 2 {
		empty, err := s.Commands.Insert(ctx, domain.Command{
			Action: domain.ActionPhoto, RoomCode: itRoomCode, DeviceID: itDeviceID,
			ActorID: "user-1", IdempotencyKey: new(""), Outcome: domain.OutcomeAccepted,
		})
		if err != nil {
			t.Fatalf("Insert(empty key %d) error = %v", i, err)
		}
		if empty.IdempotencyKey != nil {
			t.Fatalf("empty idempotency key was stored as %q, want NULL", *empty.IdempotencyKey)
		}
	}
	requireNotFound(t, func() error {
		_, err := s.Commands.ByIdempotencyKey(ctx, "")
		return err
	}(), "ByIdempotencyKey(empty)")

	// ListBySession returns one session's log in chronological order and
	// ignores session-less rows.
	firstLog, err := s.Commands.Insert(ctx, domain.Command{
		SessionID: new(session.ID), Action: domain.ActionStart, RoomCode: itRoomCode,
		DeviceID: itDeviceID, ActorID: "user-1", Outcome: domain.OutcomeAccepted,
		CreatedAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("Insert(first log) error = %v", err)
	}
	secondLog, err := s.Commands.Insert(ctx, domain.Command{
		SessionID: new(session.ID), Action: domain.ActionStop, RoomCode: itRoomCode,
		DeviceID: itDeviceID, ActorID: "user-1", Outcome: domain.OutcomeAccepted,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("Insert(second log) error = %v", err)
	}
	logs, err := s.Commands.ListBySession(ctx, session.ID)
	if err != nil || len(logs) != 2 || logs[0].ID != firstLog.ID || logs[1].ID != secondLog.ID {
		t.Fatalf("ListBySession() = %+v, err %v; want the two rows in order", sessionIDsOfCommands(logs), err)
	}

	// The CHECK constraint on the action vocabulary maps onto ErrConflict.
	requireConflict(t, func() error {
		_, err := s.Commands.Insert(ctx, domain.Command{
			Action: "bogus", RoomCode: itRoomCode, DeviceID: itDeviceID,
			ActorID: "user-1", Outcome: domain.OutcomeAccepted,
		})
		return err
	}(), "Insert(unknown action)")
}

func sessionIDsOfCommands(commands []domain.Command) []string {
	ids := make([]string, len(commands))
	for i, cmd := range commands {
		ids[i] = cmd.ID
	}
	return ids
}
