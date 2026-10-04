package timetable

import (
	"fmt"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
)

// IsBlocking reports whether an import error code blocks a commit. Teacher
// double-booking (CodeTeacherOverlap) is reported for operator review but
// never blocks; every other code — including room overlap — does.
func IsBlocking(code string) bool {
	return code != CodeTeacherOverlap
}

// Validate checks accepted rows against the target term, its period table and
// the bound rooms:
//
//   - the room must be bound and enabled,
//   - a row carrying a term_code other than the imported term is rejected,
//   - every period in period_start..period_end must be configured for the term,
//   - every week must be within 1..term.Weeks,
//   - two rows of the same import may not share a room, weekday, overlapping
//     weeks and overlapping periods (reported on the later row as
//     CodeOverlap),
//   - a teacher may not be booked twice at the same time (reported on the
//     later row as CodeTeacherOverlap; non-blocking, see IsBlocking).
//
// Errors are ordered by row and, within a row, by check. Two rows are
// compared only when both carry a well-formed week specification and a usable
// period range.
func Validate(rows []Row, term domain.Term, periods []domain.Period, rooms map[string]domain.Room) []domain.ImportError {
	errs := make([]domain.ImportError, 0)
	configured := make(map[int]struct{}, len(periods))
	for _, period := range periods {
		configured[period.PeriodNo] = struct{}{}
	}

	type rowState struct {
		weeks    uint64
		weeksOK  bool
		periodOK bool
	}
	states := make([]rowState, len(rows))

	for index, row := range rows {
		line := rowLine(row, index)

		if row.TermCode != "" && row.TermCode != term.TermCode {
			errs = append(errs, domain.ImportError{
				Row:     line,
				Column:  "term_code",
				Code:    CodeTermMismatch,
				Message: fmt.Sprintf("row belongs to term %s, not %s", row.TermCode, term.TermCode),
			})
		}

		if room, found := rooms[row.RoomCode]; !found {
			errs = append(errs, domain.ImportError{
				Row:     line,
				Column:  "room_code",
				Code:    CodeRoomNotBound,
				Message: fmt.Sprintf("room %q is not bound", row.RoomCode),
			})
		} else if !room.Enabled {
			errs = append(errs, domain.ImportError{
				Row:     line,
				Column:  "room_code",
				Code:    CodeRoomDisabled,
				Message: fmt.Sprintf("room %q is disabled", row.RoomCode),
			})
		}

		if row.PeriodStart < 1 || row.PeriodEnd < row.PeriodStart {
			errs = append(errs, domain.ImportError{
				Row:     line,
				Column:  "period_start",
				Code:    CodeInvalidRange,
				Message: fmt.Sprintf("period range %d-%d is invalid", row.PeriodStart, row.PeriodEnd),
			})
		} else {
			// The range is well-formed, so the row can still take part in
			// conflict detection even if its periods are not configured.
			states[index].periodOK = true
			missing := make([]int, 0)
			for period := row.PeriodStart; period <= row.PeriodEnd; period++ {
				if _, ok := configured[period]; !ok {
					missing = append(missing, period)
				}
			}
			if len(missing) > 0 {
				errs = append(errs, domain.ImportError{
					Row:     line,
					Column:  "period_start",
					Code:    CodePeriodNotCovered,
					Message: fmt.Sprintf("periods %s are not configured for term %s", joinInts(missing), term.TermCode),
				})
			}
		}

		weeks, err := ExpandWeeks(row.Weeks)
		if err != nil {
			errs = append(errs, domain.ImportError{
				Row:     line,
				Column:  "weeks",
				Code:    CodeInvalidWeeks,
				Message: err.Error(),
			})
			continue
		}
		states[index].weeks = weekMask(weeks)
		states[index].weeksOK = true
		outside := make([]int, 0)
		for _, week := range weeks {
			if week > term.Weeks {
				outside = append(outside, week)
			}
		}
		if len(outside) > 0 {
			errs = append(errs, domain.ImportError{
				Row:     line,
				Column:  "weeks",
				Code:    CodeWeekOutOfRange,
				Message: fmt.Sprintf("weeks %s exceed the term's %d weeks", joinInts(outside), term.Weeks),
			})
		}
	}

	// Conflict detection compares every row with the rows before it, so an
	// overlap is always reported on the later row. At most one room error and
	// one teacher error are reported per row, naming the earliest conflicting
	// row.
	roomFlagged := make([]bool, len(rows))
	teacherFlagged := make([]bool, len(rows))
	for index := range rows {
		for earlier := 0; earlier < index; earlier++ {
			if !states[index].weeksOK || !states[earlier].weeksOK || !states[index].periodOK || !states[earlier].periodOK {
				continue
			}
			if states[index].weeks&states[earlier].weeks == 0 {
				continue
			}
			if rows[index].Weekday != rows[earlier].Weekday {
				continue
			}
			if rows[index].PeriodStart > rows[earlier].PeriodEnd || rows[earlier].PeriodStart > rows[index].PeriodEnd {
				continue
			}
			line := rowLine(rows[index], index)
			other := rowLine(rows[earlier], earlier)

			if !roomFlagged[index] && rows[index].RoomCode == rows[earlier].RoomCode {
				roomFlagged[index] = true
				errs = append(errs, domain.ImportError{
					Row:    line,
					Column: "room_code",
					Code:   CodeOverlap,
					Message: fmt.Sprintf("room %s is already booked by row %d on weekday %d, periods %d-%d",
						rows[earlier].RoomCode, other, rows[earlier].Weekday, rows[earlier].PeriodStart, rows[earlier].PeriodEnd),
				})
			}
			if !teacherFlagged[index] && rows[index].TeacherUsername == rows[earlier].TeacherUsername {
				teacherFlagged[index] = true
				errs = append(errs, domain.ImportError{
					Row:     line,
					Column:  "teacher_username",
					Code:    CodeTeacherOverlap,
					Message: fmt.Sprintf("teacher %q is already booked by row %d at the same time", rows[earlier].TeacherUsername, other),
				})
			}
			if roomFlagged[index] && teacherFlagged[index] {
				break
			}
		}
	}
	return errs
}

// rowLine is the 1-based CSV line a row is reported under. Rows parsed from a
// file carry the line recorded by ParseCSV; rows built by hand fall back to
// their slice position (the header being line 1).
func rowLine(row Row, index int) int {
	if row.Line > 0 {
		return row.Line
	}
	return index + 2
}
