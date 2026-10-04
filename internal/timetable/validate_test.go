package timetable

import (
	"strings"
	"testing"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
)

const testTermCode = "2026-FALL"

func validateFixture() (domain.Term, []domain.Period, map[string]domain.Room) {
	term := domain.Term{TermCode: testTermCode, Name: "2026 Fall", Weeks: 16}
	periods := []domain.Period{
		{PeriodNo: 1, StartTime: "08:00", EndTime: "08:45"},
		{PeriodNo: 2, StartTime: "08:50", EndTime: "09:35"},
		{PeriodNo: 3, StartTime: "10:00", EndTime: "10:45"},
	}
	rooms := map[string]domain.Room{
		"A301": {RoomCode: "A301", DeviceID: "dev-a", Enabled: true},
		"B102": {RoomCode: "B102", DeviceID: "dev-b", Enabled: true},
		"C101": {RoomCode: "C101", DeviceID: "dev-c", Enabled: false},
	}
	return term, periods, rooms
}

func testRow(room, teacher string, weekday, periodStart, periodEnd int, weeks string) Row {
	return Row{
		TermCode:        testTermCode,
		CourseCode:      "CS101",
		CourseName:      "数据结构",
		TeacherUsername: teacher,
		RoomCode:        room,
		Weekday:         weekday,
		PeriodStart:     periodStart,
		PeriodEnd:       periodEnd,
		Weeks:           weeks,
	}
}

func TestValidateAcceptsCleanRows(t *testing.T) {
	term, periods, rooms := validateFixture()
	rows := []Row{
		testRow("A301", "zhangsan", 1, 1, 2, "1-16"),
		testRow("B102", "lisi", 3, 3, 3, "1-8,10-16"),
	}
	if errs := Validate(rows, term, periods, rooms); len(errs) != 0 {
		t.Fatalf("Validate() = %+v, want no errors", errs)
	}
}

// TestValidateRowChecks covers the per-row rules; the reported row number falls
// back to the slice position (line = index + 2) for hand-built rows.
func TestValidateRowChecks(t *testing.T) {
	tests := []struct {
		name     string
		row      Row
		wantCode string
		wantCol  string
		wantMsg  string
	}{
		{
			name:     "unbound room",
			row:      testRow("X999", "zhangsan", 1, 1, 2, "1-16"),
			wantCode: CodeRoomNotBound,
			wantCol:  "room_code",
			wantMsg:  "X999",
		},
		{
			name:     "disabled room",
			row:      testRow("C101", "zhangsan", 1, 1, 2, "1-16"),
			wantCode: CodeRoomDisabled,
			wantCol:  "room_code",
			wantMsg:  "C101",
		},
		{
			name: "term mismatch",
			row: Row{TermCode: "2025-SPRING", RoomCode: "A301", TeacherUsername: "zhangsan",
				Weekday: 1, PeriodStart: 1, PeriodEnd: 2, Weeks: "1-16"},
			wantCode: CodeTermMismatch,
			wantCol:  "term_code",
			wantMsg:  "2025-SPRING",
		},
		{
			name:     "period range not configured",
			row:      testRow("A301", "zhangsan", 1, 1, 4, "1-16"),
			wantCode: CodePeriodNotCovered,
			wantCol:  "period_start",
			wantMsg:  "4",
		},
		{
			name:     "weeks beyond the term",
			row:      testRow("A301", "zhangsan", 1, 1, 2, "15-17"),
			wantCode: CodeWeekOutOfRange,
			wantCol:  "weeks",
			wantMsg:  "17",
		},
		{
			name:     "malformed week specification",
			row:      testRow("A301", "zhangsan", 1, 1, 2, "bogus"),
			wantCode: CodeInvalidWeeks,
			wantCol:  "weeks",
			wantMsg:  "bogus",
		},
		{
			name: "invalid period range",
			row: Row{TermCode: testTermCode, RoomCode: "A301", TeacherUsername: "zhangsan",
				Weekday: 1, PeriodStart: 0, PeriodEnd: 2, Weeks: "1-16"},
			wantCode: CodeInvalidRange,
			wantCol:  "period_start",
			wantMsg:  "0-2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			term, periods, rooms := validateFixture()
			errs := Validate([]Row{tt.row}, term, periods, rooms)
			if len(errs) != 1 {
				t.Fatalf("Validate() = %+v, want exactly one error", errs)
			}
			got := errs[0]
			if got.Code != tt.wantCode || got.Column != tt.wantCol || got.Row != 2 {
				t.Fatalf("Validate() error = %+v, want %s/%s on row 2", got, tt.wantCode, tt.wantCol)
			}
			if !strings.Contains(got.Message, tt.wantMsg) {
				t.Fatalf("Validate() message = %q, want it to contain %q", got.Message, tt.wantMsg)
			}
		})
	}
}

// TestValidateWeekOutOfRangeListsOnlyOffendingWeeks keeps the operator report
// precise: only the weeks outside the term appear.
func TestValidateWeekOutOfRangeListsOnlyOffendingWeeks(t *testing.T) {
	term, periods, rooms := validateFixture()
	errs := Validate([]Row{testRow("A301", "zhangsan", 1, 1, 2, "15-17")}, term, periods, rooms)
	if len(errs) != 1 {
		t.Fatalf("Validate() = %+v, want one error", errs)
	}
	if message := errs[0].Message; !strings.Contains(message, "17") || strings.Contains(message, "15") {
		t.Fatalf("Validate() message = %q, want only week 17 reported", message)
	}
}

func TestValidateRoomOverlap(t *testing.T) {
	term, periods, rooms := validateFixture()
	first := testRow("A301", "zhangsan", 1, 1, 2, "1-16")
	second := testRow("A301", "lisi", 1, 2, 3, "1-16")

	errs := Validate([]Row{first, second}, term, periods, rooms)
	overlap, ok := findError(errs, CodeOverlap)
	if !ok {
		t.Fatalf("Validate() = %+v, want a room %s error", errs, CodeOverlap)
	}
	if overlap.Row != 3 || overlap.Column != "room_code" {
		t.Fatalf("overlap error = %+v, want row 3 / column room_code (reported on the later row)", overlap)
	}
	if !strings.Contains(overlap.Message, "row 2") {
		t.Fatalf("overlap message = %q, want it to name row 2", overlap.Message)
	}
}

// TestValidateOverlapReportedOnTheLaterRowOnce pins the "at most one room error
// per row, naming the earliest conflicting row" rule.
func TestValidateOverlapReportedOnTheLaterRowOnce(t *testing.T) {
	term, periods, rooms := validateFixture()
	first := testRow("A301", "teacher-one", 1, 1, 2, "1-16")
	second := testRow("A301", "teacher-two", 1, 1, 2, "1-16")
	third := testRow("A301", "teacher-three", 1, 1, 2, "1-16")
	errs := Validate([]Row{first, second, third}, term, periods, rooms)
	if len(errs) != 2 {
		t.Fatalf("Validate() = %+v, want one overlap per later row (2 errors)", errs)
	}
	if errs[0].Row != 3 || errs[1].Row != 4 {
		t.Fatalf("overlap rows = %d, %d; want 3 and 4", errs[0].Row, errs[1].Row)
	}
	for _, importErr := range errs {
		if importErr.Code != CodeOverlap || !strings.Contains(importErr.Message, "row 2") {
			t.Fatalf("overlap error = %+v, want %s naming row 2", importErr, CodeOverlap)
		}
	}
}

func TestValidateNoOverlapWhenIntervalsAreDisjoint(t *testing.T) {
	term, periods, rooms := validateFixture()
	term.Weeks = 20
	tests := []struct {
		name   string
		second Row
	}{
		{"disjoint weeks", testRow("A301", "lisi", 1, 1, 2, "9-16")},
		{"different weekday", testRow("A301", "lisi", 2, 1, 2, "1-16")},
		{"disjoint periods", testRow("A301", "lisi", 1, 3, 3, "1-16")},
	}
	first := testRow("A301", "zhangsan", 1, 1, 2, "1-8")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if errs := Validate([]Row{first, tt.second}, term, periods, rooms); len(errs) != 0 {
				t.Fatalf("Validate() = %+v, want no errors", errs)
			}
		})
	}
}

// TestValidateTeacherOverlapIsNonBlocking covers the one code that is reported
// for operator review but never blocks a commit.
func TestValidateTeacherOverlapIsNonBlocking(t *testing.T) {
	term, periods, rooms := validateFixture()
	rows := []Row{
		testRow("A301", "zhangsan", 1, 1, 2, "1-16"),
		testRow("B102", "zhangsan", 1, 1, 2, "1-16"),
	}
	errs := Validate(rows, term, periods, rooms)
	if len(errs) != 1 {
		t.Fatalf("Validate() = %+v, want exactly one teacher_overlap error", errs)
	}
	got := errs[0]
	if got.Code != CodeTeacherOverlap || got.Column != "teacher_username" || got.Row != 3 {
		t.Fatalf("Validate() error = %+v, want %s/teacher_username on row 3", got, CodeTeacherOverlap)
	}
	if IsBlocking(got.Code) {
		t.Fatalf("IsBlocking(%s) = true, want false", got.Code)
	}
	if !IsBlocking(CodeOverlap) || !IsBlocking(CodeRoomNotBound) || !IsBlocking(CodeInvalidWeeks) {
		t.Fatalf("IsBlocking reported a blocking code as non-blocking")
	}
}

// TestValidateConflictDetectionNeedsWellFormedRows ensures a row whose weeks or
// period range is already rejected cannot produce a phantom overlap.
func TestValidateConflictDetectionNeedsWellFormedRows(t *testing.T) {
	term, periods, rooms := validateFixture()
	tests := []struct {
		name  string
		first Row
	}{
		{"invalid weeks", testRow("A301", "zhangsan", 1, 1, 2, "bogus")},
		{"invalid period range", testRow("A301", "zhangsan", 1, 0, 2, "1-16")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := Validate([]Row{tt.first, testRow("A301", "zhangsan", 1, 1, 2, "1-16")}, term, periods, rooms)
			if _, ok := findError(errs, CodeOverlap); ok {
				t.Fatalf("Validate() = %+v, want no overlap involving a rejected row", errs)
			}
		})
	}
}

// TestValidateUsesCSVLineNumbers verifies that rows carrying a physical line
// number are reported under it, and hand-built rows fall back to position.
func TestValidateUsesCSVLineNumbers(t *testing.T) {
	term, periods, rooms := validateFixture()
	withLine := testRow("X999", "zhangsan", 1, 1, 2, "1-16")
	withLine.Line = 7
	errs := Validate([]Row{withLine}, term, periods, rooms)
	if len(errs) != 1 || errs[0].Row != 7 {
		t.Fatalf("Validate() = %+v, want the error on line 7", errs)
	}

	handBuilt := testRow("X999", "teacher-hand", 1, 1, 2, "1-16")
	errs = Validate([]Row{testRow("A301", "teacher-first", 1, 1, 2, "1-16"), handBuilt}, term, periods, rooms)
	if len(errs) != 1 || errs[0].Row != 3 {
		t.Fatalf("Validate() = %+v, want the error on row index 1 reported as line 3", errs)
	}
}

func TestValidateErrorsAreOrderedByRow(t *testing.T) {
	term, periods, rooms := validateFixture()
	rows := []Row{
		testRow("X999", "teacher-one", 1, 1, 2, "1-16"),
		testRow("A301", "teacher-two", 1, 1, 2, "bogus"),
		testRow("C101", "teacher-three", 1, 1, 2, "1-16"),
	}
	errs := Validate(rows, term, periods, rooms)
	if len(errs) != 3 {
		t.Fatalf("Validate() = %+v, want three errors", errs)
	}
	for i := 1; i < len(errs); i++ {
		if errs[i-1].Row > errs[i].Row {
			t.Fatalf("errors are not ordered by row: %+v", errs)
		}
	}
	if errs[0].Row != 2 || errs[1].Row != 3 || errs[2].Row != 4 {
		t.Fatalf("error rows = %v, want 2, 3, 4", []int{errs[0].Row, errs[1].Row, errs[2].Row})
	}
}
