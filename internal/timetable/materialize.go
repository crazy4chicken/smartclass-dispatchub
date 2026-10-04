package timetable

import (
	"fmt"
	"strings"
	"time"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/id"
)

// Materialize expands one accepted entry into one concrete recording session
// per teaching week. The date of week N on weekday W is
//
//	week1_monday + (N-1)*7 + (W-1) days
//
// and the session runs from periods[period_start].start_time to
// periods[period_end].end_time as wall-clock times in loc. Occurrences that
// have already started at now are emitted with Status=canceled so the record
// is kept without arming the scheduler; the rest are Status=planned.
//
// Sessions are returned oldest first and carry Origin=schedule, EntryID, a
// fresh ULID and the entry's room. DeviceID and CameraEnum are left zero: the
// caller stamps them from the room binding before persisting (recordings are
// started against the device, not the room code).
func Materialize(entry domain.Entry, term domain.Term, periods map[int]domain.Period, loc *time.Location, now time.Time) ([]domain.Session, error) {
	if loc == nil {
		loc = time.UTC
	}
	weeks, err := ExpandWeeks(entry.Weeks)
	if err != nil {
		return nil, fmt.Errorf("materialize entry %s: %w", entry.ID, err)
	}
	if entry.Weekday < 1 || entry.Weekday > 7 {
		return nil, fmt.Errorf("materialize entry %s: weekday %d is outside 1..7", entry.ID, entry.Weekday)
	}
	start, ok := periods[entry.PeriodStart]
	if !ok {
		return nil, fmt.Errorf("materialize entry %s: period %d is not configured", entry.ID, entry.PeriodStart)
	}
	end, ok := periods[entry.PeriodEnd]
	if !ok {
		return nil, fmt.Errorf("materialize entry %s: period %d is not configured", entry.ID, entry.PeriodEnd)
	}
	startHour, startMinute, err := parseClock(start.StartTime)
	if err != nil {
		return nil, fmt.Errorf("materialize entry %s: period %d start: %w", entry.ID, entry.PeriodStart, err)
	}
	endHour, endMinute, err := parseClock(end.EndTime)
	if err != nil {
		return nil, fmt.Errorf("materialize entry %s: period %d end: %w", entry.ID, entry.PeriodEnd, err)
	}

	year, month, day := term.Week1Monday.Date()
	sessions := make([]domain.Session, 0, len(weeks))
	for _, week := range weeks {
		offset := (week-1)*7 + (entry.Weekday - 1)
		startsAt := time.Date(year, month, day+offset, startHour, startMinute, 0, 0, loc)
		endsAt := time.Date(year, month, day+offset, endHour, endMinute, 0, 0, loc)
		if !endsAt.After(startsAt) {
			return nil, fmt.Errorf("materialize entry %s: period %d ends at %s, not after period %d starting at %s",
				entry.ID, entry.PeriodEnd, end.EndTime, entry.PeriodStart, start.StartTime)
		}
		status := domain.SessionPlanned
		if !startsAt.After(now) {
			status = domain.SessionCanceled
		}
		entryID := entry.ID
		sessions = append(sessions, domain.Session{
			ID:        id.New(),
			EntryID:   &entryID,
			RoomCode:  entry.RoomCode,
			StartsAt:  startsAt,
			EndsAt:    endsAt,
			Status:    status,
			Origin:    domain.OriginSchedule,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	return sessions, nil
}

// parseClock parses a "HH:MM" (or "HH:MM:SS") wall-clock time into hour and
// minute.
func parseClock(value string) (int, int, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"15:04", "15:04:05"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed.Hour(), parsed.Minute(), nil
		}
	}
	return 0, 0, fmt.Errorf("invalid clock time %q, expected HH:MM", value)
}
