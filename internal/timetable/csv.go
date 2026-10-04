package timetable

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
)

// csvColumns is the exact set of columns a timetable import must carry. The
// header is matched by name, so its order is free.
var csvColumns = []string{
	"term_code", "course_code", "course_name", "teacher_username",
	"room_code", "weekday", "period_start", "period_end", "weeks",
}

// MaxPeriod is the highest 节次 number the periods table accepts.
const MaxPeriod = 20

// Import error codes. They are stable identifiers carried in
// domain.ImportError.Code and in the API's error list.
const (
	// CodeEmptyFile marks a CSV with no header row at all.
	CodeEmptyFile = "empty_file"
	// CodeMissingColumn marks a required header column that is absent.
	CodeMissingColumn = "missing_column"
	// CodeUnknownColumn marks a header column that is not part of the contract.
	CodeUnknownColumn = "unknown_column"
	// CodeDuplicateColumn marks a header column listed twice.
	CodeDuplicateColumn = "duplicate_column"
	// CodeColumnCount marks a data row whose cell count differs from the header.
	CodeColumnCount = "column_count"
	// CodeMissingValue marks an empty required cell.
	CodeMissingValue = "missing_value"
	// CodeInvalidInt marks a cell that is not an integer.
	CodeInvalidInt = "invalid_int"
	// CodeInvalidRange marks an integer cell outside its allowed bounds.
	CodeInvalidRange = "invalid_range"
	// CodeInvalidWeeks marks a malformed week specification.
	CodeInvalidWeeks = "invalid_weeks"
	// CodeRoomNotBound marks a room_code with no room binding.
	CodeRoomNotBound = "room_not_bound"
	// CodeRoomDisabled marks a bound room that is disabled.
	CodeRoomDisabled = "room_disabled"
	// CodePeriodNotCovered marks a period range the term's periods do not cover.
	CodePeriodNotCovered = "period_not_covered"
	// CodeWeekOutOfRange marks weeks beyond the term's teaching week count.
	CodeWeekOutOfRange = "week_out_of_range"
	// CodeTermMismatch marks a row whose term_code is not the imported term.
	CodeTermMismatch = "term_mismatch"
	// CodeOverlap marks a room double-booking between two rows of one import.
	CodeOverlap = "overlap"
	// CodeTeacherOverlap marks a teacher double-booking between two rows of one
	// import. Unlike every other code it does not block a commit.
	CodeTeacherOverlap = "teacher_overlap"
)

// Row is one accepted CSV record: the schedule_entries fields plus the
// verbatim cells the row was built from.
type Row struct {
	TermCode        string
	CourseCode      string
	CourseName      string
	TeacherUsername string
	RoomCode        string
	Weeks           string
	Weekday         int
	PeriodStart     int
	PeriodEnd       int

	// Line is the 1-based CSV line number the record starts on (the header is
	// line 1). It is zero when the Row was built by hand; Validate then falls
	// back to the row's position in the slice.
	Line int

	// Raw holds every trimmed cell keyed by its column name.
	Raw map[string]string
}

// ParseCSV reads a timetable CSV. The header row is required, matched by name
// in any order (case-insensitively, surrounding whitespace trimmed), and must
// name exactly the nine contract columns. Data rows are trimmed, converted and
// range-checked; a row with any error is reported in rowErrs and omitted from
// rows. maxRows caps the number of data rows (zero or less means no cap). A
// UTF-8 BOM and CRLF line endings are tolerated.
//
// Header problems are reported as row-0 errors and suppress all rows. Errors
// returned in err are I/O or file-structure failures (a malformed CSV, more
// data rows than maxRows).
func ParseCSV(r io.Reader, maxRows int) ([]Row, []domain.ImportError, error) {
	reader := csv.NewReader(stripBOM(r))
	reader.FieldsPerRecord = -1 // ragged rows become per-row errors, not parse failures

	header, err := reader.Read()
	if err == io.EOF {
		return nil, []domain.ImportError{{
			Row:     0,
			Code:    CodeEmptyFile,
			Message: "csv file is empty, expected a header row",
		}}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read csv header: %w", err)
	}
	columns, headerErrs := mapHeader(header)
	if len(headerErrs) > 0 {
		return nil, headerErrs, nil
	}

	rows := make([]Row, 0, 16)
	var rowErrs []domain.ImportError
	dataRow := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, rowErrs, fmt.Errorf("read csv row %d: %w", dataRow+2, err)
		}
		dataRow++
		if maxRows > 0 && dataRow > maxRows {
			return nil, rowErrs, fmt.Errorf("csv has more than %d data rows", maxRows)
		}
		line, _ := reader.FieldPos(0)
		row, errs := parseRow(record, columns, line)
		if len(errs) > 0 {
			rowErrs = append(rowErrs, errs...)
			continue
		}
		rows = append(rows, row)
	}
	return rows, rowErrs, nil
}

// mapHeader resolves the header record into column positions. The returned
// errors are row-0 errors; columns is nil when any error is present.
func mapHeader(header []string) (map[string]int, []domain.ImportError) {
	wanted := make(map[string]bool, len(csvColumns))
	for _, name := range csvColumns {
		wanted[name] = true
	}
	columns := make(map[string]int, len(csvColumns))
	seen := make(map[string]bool, len(header))
	errs := make([]domain.ImportError, 0)
	for index, cell := range header {
		name := strings.ToLower(strings.TrimSpace(cell))
		if !wanted[name] {
			errs = append(errs, domain.ImportError{
				Row:     0,
				Column:  name,
				Code:    CodeUnknownColumn,
				Message: fmt.Sprintf("unknown column %q", name),
			})
			continue
		}
		if seen[name] {
			errs = append(errs, domain.ImportError{
				Row:     0,
				Column:  name,
				Code:    CodeDuplicateColumn,
				Message: fmt.Sprintf("duplicate column %q", name),
			})
			continue
		}
		seen[name] = true
		columns[name] = index
	}
	for _, name := range csvColumns {
		if !seen[name] {
			errs = append(errs, domain.ImportError{
				Row:     0,
				Column:  name,
				Code:    CodeMissingColumn,
				Message: fmt.Sprintf("missing column %q", name),
			})
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return columns, nil
}

// parseRow trims and converts one data record. It returns the row or every
// error found in it, never both.
func parseRow(record []string, columns map[string]int, line int) (Row, []domain.ImportError) {
	if len(record) != len(csvColumns) {
		return Row{}, []domain.ImportError{{
			Row:     line,
			Code:    CodeColumnCount,
			Message: fmt.Sprintf("expected %d columns, got %d", len(csvColumns), len(record)),
		}}
	}
	raw := make(map[string]string, len(csvColumns))
	for _, name := range csvColumns {
		raw[name] = strings.TrimSpace(record[columns[name]])
	}
	row := Row{
		TermCode:        raw["term_code"],
		CourseCode:      raw["course_code"],
		CourseName:      raw["course_name"],
		TeacherUsername: raw["teacher_username"],
		RoomCode:        raw["room_code"],
		Weeks:           raw["weeks"],
		Line:            line,
		Raw:             raw,
	}
	errs := make([]domain.ImportError, 0)
	for _, name := range []string{"term_code", "course_code", "course_name", "teacher_username", "room_code"} {
		if raw[name] == "" {
			errs = append(errs, domain.ImportError{
				Row:     line,
				Column:  name,
				Code:    CodeMissingValue,
				Message: fmt.Sprintf("column %q is empty", name),
			})
		}
	}

	var cellErr *domain.ImportError
	row.Weekday, cellErr = parseCellInt(raw["weekday"], "weekday", line, 1, 7)
	if cellErr != nil {
		errs = append(errs, *cellErr)
	}
	row.PeriodStart, cellErr = parseCellInt(raw["period_start"], "period_start", line, 1, MaxPeriod)
	if cellErr != nil {
		errs = append(errs, *cellErr)
	}
	row.PeriodEnd, cellErr = parseCellInt(raw["period_end"], "period_end", line, 1, MaxPeriod)
	if cellErr != nil {
		errs = append(errs, *cellErr)
	}
	if row.PeriodStart > 0 && row.PeriodEnd > 0 && row.PeriodEnd < row.PeriodStart {
		errs = append(errs, domain.ImportError{
			Row:     line,
			Column:  "period_end",
			Code:    CodeInvalidRange,
			Message: fmt.Sprintf("period_end %d is before period_start %d", row.PeriodEnd, row.PeriodStart),
		})
	}

	if raw["weeks"] == "" {
		errs = append(errs, domain.ImportError{
			Row:     line,
			Column:  "weeks",
			Code:    CodeMissingValue,
			Message: `column "weeks" is empty`,
		})
	} else if _, err := ExpandWeeks(raw["weeks"]); err != nil {
		errs = append(errs, domain.ImportError{
			Row:     line,
			Column:  "weeks",
			Code:    CodeInvalidWeeks,
			Message: err.Error(),
		})
	}

	if len(errs) > 0 {
		return Row{}, errs
	}
	return row, nil
}

// parseCellInt parses one integer cell and range-checks it.
func parseCellInt(value, column string, line, min, max int) (int, *domain.ImportError) {
	if value == "" {
		return 0, &domain.ImportError{
			Row:     line,
			Column:  column,
			Code:    CodeMissingValue,
			Message: fmt.Sprintf("column %q is empty", column),
		}
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, &domain.ImportError{
			Row:     line,
			Column:  column,
			Code:    CodeInvalidInt,
			Message: fmt.Sprintf("column %q must be an integer, got %q", column, value),
		}
	}
	if number < min || number > max {
		return 0, &domain.ImportError{
			Row:     line,
			Column:  column,
			Code:    CodeInvalidRange,
			Message: fmt.Sprintf("column %q must be between %d and %d, got %d", column, min, max, number),
		}
	}
	return number, nil
}

// stripBOM removes a leading UTF-8 byte order mark, which spreadsheet exports
// commonly prepend.
func stripBOM(r io.Reader) io.Reader {
	buffered := bufio.NewReader(r)
	if head, err := buffered.Peek(3); err == nil && bytes.Equal(head, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = buffered.Discard(3)
	}
	return buffered
}
