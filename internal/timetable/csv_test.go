package timetable

import (
	"strings"
	"testing"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
)

const (
	csvHeader = "term_code,course_code,course_name,teacher_username,room_code,weekday,period_start,period_end,weeks"
	csvRow1   = `2026-FALL,CS101,数据结构,zhangsan,A301,1,1,2,"1-16"`
	csvRow2   = `2026-FALL,MA201,高等数学,lisi,B102,3,3,4,"1-8,10-16"`
)

func csvFile(rows ...string) string {
	input := csvHeader + "\n"
	for _, row := range rows {
		input += row + "\n"
	}
	return input
}

func findError(errs []domain.ImportError, code string) (domain.ImportError, bool) {
	for _, importErr := range errs {
		if importErr.Code == code {
			return importErr, true
		}
	}
	return domain.ImportError{}, false
}

// TestParseCSVPermutedHeaderAndRowShape covers the free column order, the BOM,
// CRLF endings, blank lines and the verbatim cell map.
func TestParseCSVPermutedHeaderAndRowShape(t *testing.T) {
	input := "\xEF\xBB\xBFweeks,room_code,teacher_username,course_name,course_code,term_code,period_end,period_start,weekday\r\n" +
		"\r\n" +
		"\"1-2\",A301,zhangsan,数据结构,CS101,2026-FALL,2,1,1\r\n"

	rows, rowErrs, err := ParseCSV(strings.NewReader(input), 0)
	if err != nil {
		t.Fatalf("ParseCSV() error = %v, want nil", err)
	}
	if len(rowErrs) != 0 {
		t.Fatalf("ParseCSV() row errors = %+v, want none", rowErrs)
	}
	if len(rows) != 1 {
		t.Fatalf("ParseCSV() returned %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.TermCode != "2026-FALL" || row.CourseCode != "CS101" || row.CourseName != "数据结构" ||
		row.TeacherUsername != "zhangsan" || row.RoomCode != "A301" ||
		row.Weekday != 1 || row.PeriodStart != 1 || row.PeriodEnd != 2 || row.Weeks != "1-2" {
		t.Fatalf("ParseCSV() row = %+v, want the values of the permuted header", row)
	}
	// The blank line is skipped by the CSV reader, so the record is on line 3.
	if row.Line != 3 {
		t.Fatalf("row.Line = %d, want 3 (the physical line of the record)", row.Line)
	}
	if len(row.Raw) != 9 || row.Raw["course_name"] != "数据结构" || row.Raw["weeks"] != "1-2" {
		t.Fatalf("row.Raw = %v, want the nine trimmed cells", row.Raw)
	}

	// Header names are matched case-insensitively with surrounding whitespace
	// trimmed.
	rows, rowErrs, err = ParseCSV(strings.NewReader(
		" Term_Code , COURSE_CODE ,course_name,teacher_username,room_code,weekday,period_start,period_end,weeks\n"+csvRow1+"\n"), 0)
	if err != nil || len(rowErrs) != 0 || len(rows) != 1 {
		t.Fatalf("ParseCSV() with a messy header = %d rows, %d errors, err %v; want 1 row, 0 errors, nil", len(rows), len(rowErrs), err)
	}
	if rows[0].TermCode != "2026-FALL" || rows[0].CourseCode != "CS101" {
		t.Fatalf("ParseCSV() with a messy header row = %+v", rows[0])
	}
}

// TestParseCSVRowErrors checks that a malformed row is reported with its line,
// column and code, and is omitted from the accepted rows.
func TestParseCSVRowErrors(t *testing.T) {
	tests := []struct {
		name       string
		row        string
		wantColumn string
		wantCode   string
	}{
		{"empty required cell", `2026-FALL,,数据结构,zhangsan,A301,1,1,2,"1-16"`, "course_code", CodeMissingValue},
		{"non integer weekday", `2026-FALL,CS101,数据结构,zhangsan,A301,x,1,2,"1-16"`, "weekday", CodeInvalidInt},
		{"weekday out of range", `2026-FALL,CS101,数据结构,zhangsan,A301,8,1,2,"1-16"`, "weekday", CodeInvalidRange},
		{"period start below one", `2026-FALL,CS101,数据结构,zhangsan,A301,1,0,2,"1-16"`, "period_start", CodeInvalidRange},
		{"period end below start", `2026-FALL,CS101,数据结构,zhangsan,A301,1,5,3,"1-16"`, "period_end", CodeInvalidRange},
		{"period end above maximum", `2026-FALL,CS101,数据结构,zhangsan,A301,1,1,21,"1-16"`, "period_end", CodeInvalidRange},
		{"missing weeks", `2026-FALL,CS101,数据结构,zhangsan,A301,1,1,2,`, "weeks", CodeMissingValue},
		{"malformed weeks", `2026-FALL,CS101,数据结构,zhangsan,A301,1,1,2,1-0`, "weeks", CodeInvalidWeeks},
		{"too few cells", `2026-FALL,CS101,数据结构,zhangsan,A301,1,1`, "", CodeColumnCount},
		{"too many cells", csvRow1 + `,extra`, "", CodeColumnCount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, rowErrs, err := ParseCSV(strings.NewReader(csvFile(tt.row)), 0)
			if err != nil {
				t.Fatalf("ParseCSV() error = %v, want nil", err)
			}
			if len(rows) != 0 {
				t.Fatalf("ParseCSV() returned %d rows for a bad row, want 0", len(rows))
			}
			got, ok := findError(rowErrs, tt.wantCode)
			if !ok {
				t.Fatalf("ParseCSV() errors = %+v, want code %q", rowErrs, tt.wantCode)
			}
			if got.Row != 2 {
				t.Fatalf("error row = %d, want 2", got.Row)
			}
			if tt.wantColumn != "" && got.Column != tt.wantColumn {
				t.Fatalf("error column = %q, want %q", got.Column, tt.wantColumn)
			}
			if got.Message == "" {
				t.Fatalf("error %+v has an empty message", got)
			}
		})
	}
}

// TestParseCSVRowReportsEveryProblemOneRowAtATime keeps the "full error list"
// import contract: one bad row contributes every error it carries, and a good
// sibling row is still accepted.
func TestParseCSVRowReportsEveryProblemOneRowAtATime(t *testing.T) {
	bad := `2025-SPRING,,数据结构,zhangsan,A301,9,0,2,bogus`
	rows, rowErrs, err := ParseCSV(strings.NewReader(csvFile(bad, csvRow1)), 0)
	if err != nil {
		t.Fatalf("ParseCSV() error = %v, want nil", err)
	}
	if len(rows) != 1 || rows[0].CourseCode != "CS101" {
		t.Fatalf("ParseCSV() rows = %+v, want only the good row", rows)
	}
	if len(rowErrs) < 4 {
		t.Fatalf("ParseCSV() errors = %+v, want one error per bad cell", rowErrs)
	}
	for _, want := range []string{CodeMissingValue, CodeInvalidRange, CodeInvalidWeeks} {
		if _, ok := findError(rowErrs, want); !ok {
			t.Fatalf("ParseCSV() errors = %+v, want code %q", rowErrs, want)
		}
	}
	for _, importErr := range rowErrs {
		if importErr.Row != 2 {
			t.Fatalf("error %+v is not attributed to line 2", importErr)
		}
	}
}

// TestParseCSVHeaderErrors covers rejection of unknown, missing and duplicate
// columns; header problems suppress every data row.
func TestParseCSVHeaderErrors(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		wantColumn string
		wantCode   string
		wantAny    []string
	}{
		{
			name:       "unknown column",
			header:     csvHeader + ",term_name",
			wantColumn: "term_name",
			wantCode:   CodeUnknownColumn,
		},
		{
			name:       "missing column",
			header:     "term_code,course_code,course_name,teacher_username,room_code,weekday,period_start,period_end",
			wantColumn: "weeks",
			wantCode:   CodeMissingColumn,
		},
		{
			name:       "duplicate column",
			header:     csvHeader + ",weeks",
			wantColumn: "weeks",
			wantCode:   CodeDuplicateColumn,
		},
		{
			name:       "unknown and missing together",
			header:     "term_code,course_code,course_name,teacher_username,room_code,weekday,period_start,period_end,term_name",
			wantColumn: "term_name",
			wantCode:   CodeUnknownColumn,
			wantAny:    []string{CodeMissingColumn},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, rowErrs, err := ParseCSV(strings.NewReader(tt.header+"\n"+csvRow1+"\n"), 0)
			if err != nil {
				t.Fatalf("ParseCSV() error = %v, want nil", err)
			}
			if len(rows) != 0 {
				t.Fatalf("ParseCSV() returned %d rows, want 0 when the header is rejected", len(rows))
			}
			got, ok := findError(rowErrs, tt.wantCode)
			if !ok || got.Column != tt.wantColumn {
				t.Fatalf("ParseCSV() errors = %+v, want %s/%s", rowErrs, tt.wantCode, tt.wantColumn)
			}
			for _, code := range tt.wantAny {
				if _, ok := findError(rowErrs, code); !ok {
					t.Fatalf("ParseCSV() errors = %+v, want code %q", rowErrs, code)
				}
			}
			for _, importErr := range rowErrs {
				if importErr.Row != 0 {
					t.Fatalf("header error %+v is not attributed to row 0", importErr)
				}
			}
		})
	}
}

func TestParseCSVEmptyFile(t *testing.T) {
	rows, rowErrs, err := ParseCSV(strings.NewReader(""), 0)
	if err != nil {
		t.Fatalf("ParseCSV() error = %v, want nil", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ParseCSV() returned %d rows for an empty file", len(rows))
	}
	if len(rowErrs) != 1 || rowErrs[0].Code != CodeEmptyFile || rowErrs[0].Row != 0 {
		t.Fatalf("ParseCSV() errors = %+v, want one row-0 %s error", rowErrs, CodeEmptyFile)
	}
}

// TestParseCSVMaxRows pins the boundary: exactly maxRows rows pass, one more is
// a file-structure failure.
func TestParseCSVMaxRows(t *testing.T) {
	input := csvFile(csvRow1, csvRow2)

	rows, rowErrs, err := ParseCSV(strings.NewReader(input), 2)
	if err != nil {
		t.Fatalf("ParseCSV(maxRows=2) error = %v, want nil", err)
	}
	if len(rows) != 2 || len(rowErrs) != 0 {
		t.Fatalf("ParseCSV(maxRows=2) = %d rows, %d errors; want 2 rows, 0 errors", len(rows), len(rowErrs))
	}

	rows, _, err = ParseCSV(strings.NewReader(input), 1)
	if err == nil {
		t.Fatalf("ParseCSV(maxRows=1) error = nil, want a row-cap error")
	}
	if !strings.Contains(err.Error(), "more than 1") {
		t.Fatalf("ParseCSV(maxRows=1) error = %q, want it to mention the cap", err)
	}
	if rows != nil {
		t.Fatalf("ParseCSV(maxRows=1) returned %d rows alongside the error", len(rows))
	}

	rows, _, err = ParseCSV(strings.NewReader(input), 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ParseCSV(maxRows=0) = %d rows, err %v; want 2 rows and no cap", len(rows), err)
	}
}

func TestParseCSVMalformedStructure(t *testing.T) {
	rows, _, err := ParseCSV(strings.NewReader(csvHeader+"\n\"unterminated\n"), 0)
	if err == nil {
		t.Fatalf("ParseCSV() error = nil, want a CSV structure error")
	}
	if rows != nil {
		t.Fatalf("ParseCSV() returned %d rows alongside the structure error", len(rows))
	}
}

// TestParseCSVDoesNotReturnErroredRows guards the documented contract that a
// rejected row never reaches the materializer.
func TestParseCSVDoesNotReturnErroredRows(t *testing.T) {
	rows, rowErrs, err := ParseCSV(strings.NewReader(csvFile(csvRow1, `2026-FALL,CS101,数据结构,zhangsan,A301,1,1,2,bogus`)), 0)
	if err != nil {
		t.Fatalf("ParseCSV() error = %v, want nil", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ParseCSV() returned %d rows, want only the valid row", len(rows))
	}
	if _, ok := findError(rowErrs, CodeInvalidWeeks); !ok {
		t.Fatalf("ParseCSV() errors = %+v, want %s", rowErrs, CodeInvalidWeeks)
	}
}
