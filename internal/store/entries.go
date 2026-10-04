package store

import (
	"context"
	"fmt"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Entries is the schedule_entries sub-store. A zero value is not usable; Open
// assembles it with the shared pool.
type Entries struct{ db dbtx }

// scanEntry reads one schedule_entries row.
func scanEntry(row pgx.Row) (domain.Entry, error) {
	var (
		entry     domain.Entry
		teacherID pgtype.Text
	)
	if err := row.Scan(&entry.ID, &entry.ImportID, &entry.TermCode, &entry.CourseCode,
		&entry.CourseName, &entry.TeacherUsername, &teacherID, &entry.RoomCode, &entry.Weekday,
		&entry.PeriodStart, &entry.PeriodEnd, &entry.Weeks, &entry.CreatedAt); err != nil {
		return domain.Entry{}, err
	}
	entry.TeacherUserID = textOrNil(teacherID)
	return entry, nil
}

const entryColumns = `id, import_id, term_code, course_code, course_name, teacher_username,
teacher_user_id, room_code, weekday, period_start, period_end, weeks, created_at`

// ListByImport returns every entry of one import batch.
func (e *Entries) ListByImport(ctx context.Context, importID string) ([]domain.Entry, error) {
	query := `SELECT ` + entryColumns + ` FROM schedule_entries WHERE import_id = $1 ORDER BY created_at, id`
	return e.listEntries(ctx, query, "list entries for import "+importID, importID)
}

// ListByTerm returns a term's live entries: those not superseded by a later
// replace-import.
func (e *Entries) ListByTerm(ctx context.Context, termCode string) ([]domain.Entry, error) {
	query := `SELECT ` + entryColumns + ` FROM schedule_entries WHERE term_code = $1 AND superseded_by IS NULL ORDER BY created_at, id`
	return e.listEntries(ctx, query, "list entries for term "+termCode, termCode)
}

// SetTeacherUserID caches the teamusers subject resolved for a teacher
// username. A missing entry is ErrNotFound.
func (e *Entries) SetTeacherUserID(ctx context.Context, entryID, userID string) error {
	tag, err := e.db.Exec(ctx, `UPDATE schedule_entries SET teacher_user_id = $2 WHERE id = $1`, entryID, userID)
	if err != nil {
		return fmt.Errorf("set teacher for entry %s: %w", entryID, classify(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set teacher for entry %s: %w", entryID, ErrNotFound)
	}
	return nil
}

// listEntries runs an entry select and drains it.
func (e *Entries) listEntries(ctx context.Context, query, operation string, args ...any) ([]domain.Entry, error) {
	rows, err := e.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", operation, classify(err))
	}
	defer rows.Close()
	entries := make([]domain.Entry, 0, 32)
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", operation, classify(err))
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", operation, classify(err))
	}
	return entries, nil
}
