package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Terms is the terms and periods sub-store. A zero value is not usable; Open
// assembles it with the shared pool.
type Terms struct{ db dbtx }

// scanTerm reads one terms row.
func scanTerm(row pgx.Row) (domain.Term, error) {
	var term domain.Term
	if err := row.Scan(&term.TermCode, &term.Name, &term.Week1Monday, &term.Weeks, &term.CreatedAt, &term.UpdatedAt); err != nil {
		return domain.Term{}, err
	}
	return term, nil
}

// List returns every term ordered by term_code.
func (t *Terms) List(ctx context.Context) ([]domain.Term, error) {
	const query = `
SELECT term_code, name, week1_monday, weeks, created_at, updated_at
FROM terms
ORDER BY term_code`
	rows, err := t.db.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list terms: %w", classify(err))
	}
	defer rows.Close()
	terms := make([]domain.Term, 0, 8)
	for rows.Next() {
		term, err := scanTerm(rows)
		if err != nil {
			return nil, fmt.Errorf("list terms: %w", classify(err))
		}
		terms = append(terms, term)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list terms: %w", classify(err))
	}
	return terms, nil
}

// Get returns one term by code, or ErrNotFound.
func (t *Terms) Get(ctx context.Context, termCode string) (domain.Term, error) {
	const query = `
SELECT term_code, name, week1_monday, weeks, created_at, updated_at
FROM terms
WHERE term_code = $1`
	term, err := scanTerm(t.db.QueryRow(ctx, query, termCode))
	if err != nil {
		return domain.Term{}, fmt.Errorf("get term %s: %w", termCode, classify(err))
	}
	return term, nil
}

// Upsert inserts or updates a term and returns the stored row. week1_monday is
// normalized to the calendar date the caller's time falls on, stored as a UTC
// midnight DATE.
func (t *Terms) Upsert(ctx context.Context, term domain.Term) (domain.Term, error) {
	const query = `
INSERT INTO terms (term_code, name, week1_monday, weeks)
VALUES ($1, $2, $3, $4)
ON CONFLICT (term_code) DO UPDATE SET
    name = EXCLUDED.name,
    week1_monday = EXCLUDED.week1_monday,
    weeks = EXCLUDED.weeks,
    updated_at = now()
RETURNING term_code, name, week1_monday, weeks, created_at, updated_at`
	stored, err := scanTerm(t.db.QueryRow(ctx, query,
		term.TermCode, term.Name, dateOnlyUTC(term.Week1Monday), term.Weeks))
	if err != nil {
		return domain.Term{}, fmt.Errorf("upsert term %s: %w", term.TermCode, classify(err))
	}
	return stored, nil
}

// Periods returns a term's 节次 table ordered by period_no. Times render as
// "HH:MM".
func (t *Terms) Periods(ctx context.Context, termCode string) ([]domain.Period, error) {
	const query = `
SELECT period_no, left(start_time::text, 5), left(end_time::text, 5)
FROM periods
WHERE term_code = $1
ORDER BY period_no`
	rows, err := t.db.Query(ctx, query, termCode)
	if err != nil {
		return nil, fmt.Errorf("list periods for term %s: %w", termCode, classify(err))
	}
	defer rows.Close()
	periods := make([]domain.Period, 0, 12)
	for rows.Next() {
		var period domain.Period
		if err := rows.Scan(&period.PeriodNo, &period.StartTime, &period.EndTime); err != nil {
			return nil, fmt.Errorf("list periods for term %s: %w", termCode, classify(err))
		}
		periods = append(periods, period)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list periods for term %s: %w", termCode, classify(err))
	}
	return periods, nil
}

// ReplacePeriods replaces a term's whole 节次 table in one transaction. Invalid
// clock values or period numbers fail with ErrValidation before any change is
// committed.
func (t *Terms) ReplacePeriods(ctx context.Context, termCode string, periods []domain.Period) error {
	values := make([]pgtype.Time, 0, len(periods)*2)
	for _, period := range periods {
		if period.PeriodNo < 1 || period.PeriodNo > 20 {
			return fmt.Errorf("replace periods for term %s: period_no %d is outside 1..20: %w", termCode, period.PeriodNo, ErrValidation)
		}
		start, err := clockValue(period.StartTime)
		if err != nil {
			return fmt.Errorf("replace periods for term %s: period %d start_time: %w", termCode, period.PeriodNo, err)
		}
		end, err := clockValue(period.EndTime)
		if err != nil {
			return fmt.Errorf("replace periods for term %s: period %d end_time: %w", termCode, period.PeriodNo, err)
		}
		if end.Microseconds <= start.Microseconds {
			return fmt.Errorf("replace periods for term %s: period %d ends at %s, not after %s: %w",
				termCode, period.PeriodNo, period.EndTime, period.StartTime, ErrValidation)
		}
		values = append(values, start, end)
	}

	tx, err := t.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("replace periods for term %s: %w", termCode, classify(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM terms WHERE term_code = $1)`, termCode).Scan(&exists); err != nil {
		return fmt.Errorf("replace periods for term %s: %w", termCode, classify(err))
	}
	if !exists {
		return fmt.Errorf("replace periods for term %s: %w", termCode, ErrNotFound)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM periods WHERE term_code = $1`, termCode); err != nil {
		return fmt.Errorf("replace periods for term %s: %w", termCode, classify(err))
	}
	const insert = `INSERT INTO periods (term_code, period_no, start_time, end_time) VALUES ($1, $2, $3, $4)`
	for index, period := range periods {
		if _, err := tx.Exec(ctx, insert, termCode, period.PeriodNo, values[index*2], values[index*2+1]); err != nil {
			return fmt.Errorf("replace periods for term %s: insert period %d: %w", termCode, period.PeriodNo, classify(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("replace periods for term %s: %w", termCode, classify(err))
	}
	return nil
}

// dateOnlyUTC normalizes a time to the UTC midnight of the calendar date it
// falls on, matching the DATE representation of terms.week1_monday.
func dateOnlyUTC(value time.Time) time.Time {
	year, month, day := value.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// clockValue parses "HH:MM" (or "HH:MM:SS") into PostgreSQL's time-of-day.
func clockValue(value string) (pgtype.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"15:04", "15:04:05"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			microseconds := int64(parsed.Hour())*int64(time.Hour/time.Microsecond) +
				int64(parsed.Minute())*int64(time.Minute/time.Microsecond) +
				int64(parsed.Second())*int64(time.Second/time.Microsecond)
			return pgtype.Time{Microseconds: microseconds, Valid: true}, nil
		}
	}
	return pgtype.Time{}, fmt.Errorf("invalid clock value %q, expected HH:MM: %w", value, ErrValidation)
}
