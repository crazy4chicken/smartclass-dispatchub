package timetable

import (
	"strings"
	"testing"
	"time"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
)

var week1Monday = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC) // a Monday

func materializeFixture() (domain.Entry, domain.Term, map[int]domain.Period) {
	entry := domain.Entry{
		ID:          "entry-01",
		TermCode:    testTermCode,
		RoomCode:    "A301",
		Weekday:     1,
		PeriodStart: 1,
		PeriodEnd:   2,
		Weeks:       "1,3",
	}
	term := domain.Term{TermCode: testTermCode, Name: "2026 Fall", Week1Monday: week1Monday, Weeks: 16}
	periods := map[int]domain.Period{
		1: {PeriodNo: 1, StartTime: "08:00", EndTime: "08:45"},
		2: {PeriodNo: 2, StartTime: "08:50", EndTime: "09:35"},
		3: {PeriodNo: 3, StartTime: "10:00", EndTime: "10:45"},
	}
	return entry, term, periods
}

func mustMaterialize(t *testing.T, entry domain.Entry, term domain.Term, periods map[int]domain.Period, loc *time.Location, now time.Time) []domain.Session {
	t.Helper()
	sessions, err := Materialize(entry, term, periods, loc, now)
	if err != nil {
		t.Fatalf("Materialize() error = %v, want nil", err)
	}
	return sessions
}

// TestMaterializeWeekArithmetic pins the date formula
// week1_monday + (week-1)*7 + (weekday-1) days and the period clock.
func TestMaterializeWeekArithmetic(t *testing.T) {
	entry, term, periods := materializeFixture()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	sessions := mustMaterialize(t, entry, term, periods, time.UTC, now)
	if len(sessions) != 2 {
		t.Fatalf("Materialize() returned %d sessions, want one per week (2)", len(sessions))
	}

	first, second := sessions[0], sessions[1]
	if !first.StartsAt.Equal(time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("week 1 starts_at = %s, want 2026-03-02T08:00:00Z", first.StartsAt)
	}
	if !first.EndsAt.Equal(time.Date(2026, 3, 2, 9, 35, 0, 0, time.UTC)) {
		t.Fatalf("week 1 ends_at = %s, want 2026-03-02T09:35:00Z", first.EndsAt)
	}
	if !second.StartsAt.Equal(time.Date(2026, 3, 16, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("week 3 starts_at = %s, want 2026-03-16T08:00:00Z", second.StartsAt)
	}
	if !second.StartsAt.After(first.StartsAt) {
		t.Fatalf("sessions are not returned oldest first: %s then %s", first.StartsAt, second.StartsAt)
	}

	seen := map[string]bool{}
	for _, session := range sessions {
		if session.ID == "" || seen[session.ID] {
			t.Fatalf("session id %q is empty or duplicated", session.ID)
		}
		seen[session.ID] = true
		if session.EntryID == nil || *session.EntryID != entry.ID {
			t.Fatalf("session entry_id = %v, want %q", session.EntryID, entry.ID)
		}
		if session.RoomCode != entry.RoomCode || session.Origin != domain.OriginSchedule {
			t.Fatalf("session = %+v, want room %q and origin %q", session, entry.RoomCode, domain.OriginSchedule)
		}
		if session.DeviceID != "" || session.CameraEnum != 0 {
			t.Fatalf("session device binding = %q/%d, want it left for the caller to stamp", session.DeviceID, session.CameraEnum)
		}
		if !session.CreatedAt.Equal(now) || !session.UpdatedAt.Equal(now) {
			t.Fatalf("session timestamps = %s/%s, want %s", session.CreatedAt, session.UpdatedAt, now)
		}
	}
}

// TestMaterializeWeekdayOffset checks every weekday maps onto its own calendar
// day: Monday of week 1 plus (weekday-1) days.
func TestMaterializeWeekdayOffset(t *testing.T) {
	_, term, periods := materializeFixture()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		weekday int
		want    time.Time
	}{
		{1, time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)},
		{2, time.Date(2026, 3, 3, 8, 0, 0, 0, time.UTC)},
		{3, time.Date(2026, 3, 4, 8, 0, 0, 0, time.UTC)},
		{4, time.Date(2026, 3, 5, 8, 0, 0, 0, time.UTC)},
		{5, time.Date(2026, 3, 6, 8, 0, 0, 0, time.UTC)},
		{6, time.Date(2026, 3, 7, 8, 0, 0, 0, time.UTC)},
		{7, time.Date(2026, 3, 8, 8, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		entry := domain.Entry{ID: "entry-weekday", RoomCode: "A301", Weekday: tt.weekday, PeriodStart: 1, PeriodEnd: 1, Weeks: "1"}
		sessions := mustMaterialize(t, entry, term, periods, time.UTC, now)
		if len(sessions) != 1 || !sessions[0].StartsAt.Equal(tt.want) {
			t.Fatalf("weekday %d starts_at = %v, want %s", tt.weekday, sessions, tt.want)
		}
	}
}

// TestMaterializeWallClockInLocation proves the clock times are wall-clock in
// the configured zone, not instants: 08:00 stays 08:00 in +08:00.
func TestMaterializeWallClockInLocation(t *testing.T) {
	entry, term, periods := materializeFixture()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	loc := time.FixedZone("CST", 8*3600)

	sessions := mustMaterialize(t, entry, term, periods, loc, now)
	if got := sessions[0].StartsAt.Format(time.RFC3339); got != "2026-03-02T08:00:00+08:00" {
		t.Fatalf("starts_at = %s, want 2026-03-02T08:00:00+08:00", got)
	}
	if !sessions[0].StartsAt.UTC().Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("starts_at in UTC = %s, want 2026-03-02T00:00:00Z", sessions[0].StartsAt.UTC())
	}
	if sessions[0].StartsAt.Location() != loc {
		t.Fatalf("starts_at location = %s, want %s", sessions[0].StartsAt.Location(), loc)
	}

	// A nil location falls back to UTC rather than panicking.
	sessions = mustMaterialize(t, entry, term, periods, nil, now)
	if got := sessions[0].StartsAt.Format(time.RFC3339); got != "2026-03-02T08:00:00Z" {
		t.Fatalf("starts_at with a nil location = %s, want 2026-03-02T08:00:00Z", got)
	}
}

// TestMaterializePastOccurrencesAreCanceled covers the planned/canceled split:
// an occurrence that already started is kept for the record but never armed.
func TestMaterializePastOccurrencesAreCanceled(t *testing.T) {
	entry, term, periods := materializeFixture()

	tests := []struct {
		name string
		now  time.Time
		want []string
	}{
		{
			name: "all occurrences in the future",
			now:  time.Date(2026, 3, 2, 7, 59, 59, 0, time.UTC),
			want: []string{domain.SessionPlanned, domain.SessionPlanned},
		},
		{
			name: "boundary: an occurrence starting exactly now is past",
			now:  time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC),
			want: []string{domain.SessionCanceled, domain.SessionPlanned},
		},
		{
			name: "all occurrences in the past",
			now:  time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			want: []string{domain.SessionCanceled, domain.SessionCanceled},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := mustMaterialize(t, entry, term, periods, time.UTC, tt.now)
			if len(sessions) != len(tt.want) {
				t.Fatalf("Materialize() returned %d sessions, want %d", len(sessions), len(tt.want))
			}
			for i, want := range tt.want {
				if sessions[i].Status != want {
					t.Fatalf("session %d status = %q, want %q", i, sessions[i].Status, want)
				}
			}
		})
	}
}

func TestMaterializeRejectsBadEntries(t *testing.T) {
	_, term, periods := materializeFixture()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		entry   domain.Entry
		periods map[int]domain.Period
	}{
		{
			name:    "empty weeks",
			entry:   domain.Entry{ID: "e", RoomCode: "A301", Weekday: 1, PeriodStart: 1, PeriodEnd: 2, Weeks: ""},
			periods: periods,
		},
		{
			name:    "reversed weeks",
			entry:   domain.Entry{ID: "e", RoomCode: "A301", Weekday: 1, PeriodStart: 1, PeriodEnd: 2, Weeks: "3-1"},
			periods: periods,
		},
		{
			name:    "weekday zero",
			entry:   domain.Entry{ID: "e", RoomCode: "A301", Weekday: 0, PeriodStart: 1, PeriodEnd: 2, Weeks: "1"},
			periods: periods,
		},
		{
			name:    "weekday eight",
			entry:   domain.Entry{ID: "e", RoomCode: "A301", Weekday: 8, PeriodStart: 1, PeriodEnd: 2, Weeks: "1"},
			periods: periods,
		},
		{
			name:    "start period not configured",
			entry:   domain.Entry{ID: "e", RoomCode: "A301", Weekday: 1, PeriodStart: 9, PeriodEnd: 9, Weeks: "1"},
			periods: periods,
		},
		{
			name:    "end period not configured",
			entry:   domain.Entry{ID: "e", RoomCode: "A301", Weekday: 1, PeriodStart: 1, PeriodEnd: 9, Weeks: "1"},
			periods: periods,
		},
		{
			name:    "invalid clock",
			entry:   domain.Entry{ID: "e", RoomCode: "A301", Weekday: 1, PeriodStart: 1, PeriodEnd: 1, Weeks: "1"},
			periods: map[int]domain.Period{1: {PeriodNo: 1, StartTime: "25:00", EndTime: "26:00"}},
		},
		{
			name:    "period ends before it starts",
			entry:   domain.Entry{ID: "e", RoomCode: "A301", Weekday: 1, PeriodStart: 1, PeriodEnd: 1, Weeks: "1"},
			periods: map[int]domain.Period{1: {PeriodNo: 1, StartTime: "09:00", EndTime: "08:00"}},
		},
		{
			name:    "period ends when it starts",
			entry:   domain.Entry{ID: "e", RoomCode: "A301", Weekday: 1, PeriodStart: 1, PeriodEnd: 1, Weeks: "1"},
			periods: map[int]domain.Period{1: {PeriodNo: 1, StartTime: "09:00", EndTime: "09:00"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions, err := Materialize(tt.entry, term, tt.periods, time.UTC, now)
			if err == nil {
				t.Fatalf("Materialize() = %d sessions, want an error", len(sessions))
			}
			if sessions != nil {
				t.Fatalf("Materialize() returned sessions alongside the error: %+v", sessions)
			}
			if !strings.Contains(err.Error(), "materialize entry") {
				t.Fatalf("Materialize() error = %q, want it to name the entry", err)
			}
		})
	}
}
