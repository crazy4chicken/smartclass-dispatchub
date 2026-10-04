package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crazy4chicken/smartclass-dispatchub/internal/domain"
	"github.com/crazy4chicken/smartclass-dispatchub/internal/id"
	"github.com/jackc/pgx/v5"
)

// Import modes (imports.mode).
const (
	// ModeReplace supersedes the term's previous entries and cancels their
	// future planned sessions before writing the new ones.
	ModeReplace = "replace"
	// ModeAppend adds the batch without touching previous entries.
	ModeAppend = "append"
)

// defaultImportListLimit bounds Imports.List when the caller passes no limit.
const defaultImportListLimit = 100

// Imports is the import-batch sub-store. A zero value is not usable; Open
// assembles it with the shared pool.
type Imports struct{ db dbtx }

// importColumns is the column list every imports select shares.
const importColumns = `id, term_code, mode, filename, sha256, row_count, ok_count, error_count,
errors, status, imported_by, created_at`

// importInsertSQL writes one batch; callers append " RETURNING <columns>" when
// they need the stored row.
const importInsertSQL = `
INSERT INTO imports (id, term_code, mode, filename, sha256, row_count, ok_count, error_count, errors, status, imported_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

// entryInsertSQL writes one schedule_entries row; raw is stored as its empty
// object because domain.Entry does not carry the verbatim cells.
const entryInsertSQL = `
INSERT INTO schedule_entries (id, import_id, term_code, course_code, course_name, teacher_username,
teacher_user_id, room_code, weekday, period_start, period_end, weeks, raw)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, '{}'::jsonb)`

// importSessionInsertSQL writes one scheduled session.
const importSessionInsertSQL = `
INSERT INTO sessions (id, entry_id, room_code, device_id, camera_enum, starts_at, ends_at,
status, origin, stream_id, started_at, stopped_at, retry_count, last_error)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

// scanImport reads one imports row.
func scanImport(row pgx.Row) (domain.Import, error) {
	var (
		imp       domain.Import
		rawErrors []byte
	)
	if err := row.Scan(&imp.ID, &imp.TermCode, &imp.Mode, &imp.Filename, &imp.SHA256,
		&imp.RowCount, &imp.OKCount, &imp.ErrorCount, &rawErrors, &imp.Status,
		&imp.ImportedBy, &imp.CreatedAt); err != nil {
		return domain.Import{}, err
	}
	if len(rawErrors) > 0 {
		if err := json.Unmarshal(rawErrors, &imp.Errors); err != nil {
			return domain.Import{}, fmt.Errorf("decode errors column: %w", err)
		}
	}
	if imp.Errors == nil {
		imp.Errors = []domain.ImportError{}
	}
	return imp, nil
}

// Create inserts one batch (a dry run or a rejected upload) and returns the
// stored row. A batch without an id gets a fresh ULID.
func (i *Imports) Create(ctx context.Context, imp domain.Import) (domain.Import, error) {
	if imp.ID == "" {
		imp.ID = id.New()
	}
	payload, err := marshalImportErrors(imp.Errors)
	if err != nil {
		return domain.Import{}, fmt.Errorf("create import %s: %w", imp.ID, err)
	}
	const query = importInsertSQL + ` RETURNING ` + importColumns
	stored, err := scanImport(i.db.QueryRow(ctx, query, importInsertArgs(imp, payload)...))
	if err != nil {
		return domain.Import{}, fmt.Errorf("create import %s: %w", imp.ID, classify(err))
	}
	return stored, nil
}

// Get returns one batch by id, or ErrNotFound.
func (i *Imports) Get(ctx context.Context, importID string) (domain.Import, error) {
	const query = `SELECT ` + importColumns + ` FROM imports WHERE id = $1`
	imp, err := scanImport(i.db.QueryRow(ctx, query, importID))
	if err != nil {
		return domain.Import{}, fmt.Errorf("get import %s: %w", importID, classify(err))
	}
	return imp, nil
}

// List returns the import history newest first. An empty termCode lists every
// term; a limit of zero or less uses defaultImportListLimit.
func (i *Imports) List(ctx context.Context, termCode string, limit int) ([]domain.Import, error) {
	if limit <= 0 {
		limit = defaultImportListLimit
	}
	const query = `
SELECT ` + importColumns + `
FROM imports
WHERE ($1 = '' OR term_code = $1)
ORDER BY created_at DESC, id DESC
LIMIT $2`
	rows, err := i.db.Query(ctx, query, termCode, limit)
	if err != nil {
		return nil, fmt.Errorf("list imports: %w", classify(err))
	}
	defer rows.Close()
	imports := make([]domain.Import, 0, 16)
	for rows.Next() {
		imp, err := scanImport(rows)
		if err != nil {
			return nil, fmt.Errorf("list imports: %w", classify(err))
		}
		imports = append(imports, imp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list imports: %w", classify(err))
	}
	return imports, nil
}

// FindBySHA returns the most recent committed batch for a term and file hash,
// or ErrNotFound. Failed and dry-run batches are ignored so a corrected setup
// can retry the same file.
func (i *Imports) FindBySHA(ctx context.Context, termCode, sha256 string) (domain.Import, error) {
	const query = `
SELECT ` + importColumns + `
FROM imports
WHERE term_code = $1 AND sha256 = $2 AND status = 'committed'
ORDER BY created_at DESC, id DESC
LIMIT 1`
	imp, err := scanImport(i.db.QueryRow(ctx, query, termCode, sha256))
	if err != nil {
		return domain.Import{}, fmt.Errorf("find import %s/%s: %w", termCode, sha256, classify(err))
	}
	return imp, nil
}

// Commit writes the batch, its entries and its materialized sessions in one
// transaction. In replace mode it first marks the term's live entries
// superseded_by=imp.ID and cancels their future planned sessions; sessions
// already starting, recording, stopping or completed are never touched.
// Concurrent imports of the same term serialize on a transaction-scoped
// advisory lock.
//
// Entries and sessions must carry their ids (Materialize generates session
// ids; the caller generates entry ids) and sessions must carry the room's
// device_id/camera_enum; violations fail with ErrValidation before any write.
func (i *Imports) Commit(ctx context.Context, imp domain.Import, entries []domain.Entry, sessions []domain.Session) error {
	if imp.ID == "" {
		imp.ID = id.New()
	}
	for index := range entries {
		if entries[index].ID == "" {
			return fmt.Errorf("commit import %s: entry %d has no id: %w", imp.ID, index, ErrValidation)
		}
	}
	for index := range sessions {
		if sessions[index].ID == "" {
			return fmt.Errorf("commit import %s: session %d has no id: %w", imp.ID, index, ErrValidation)
		}
		if sessions[index].DeviceID == "" {
			return fmt.Errorf("commit import %s: session %s has no device_id: %w", imp.ID, sessions[index].ID, ErrValidation)
		}
	}
	payload, err := marshalImportErrors(imp.Errors)
	if err != nil {
		return fmt.Errorf("commit import %s: %w", imp.ID, err)
	}

	tx, err := i.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("commit import %s: %w", imp.ID, classify(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serializes concurrent imports of one term for the whole transaction.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "dispatch-import:"+imp.TermCode); err != nil {
		return fmt.Errorf("commit import %s: lock term %s: %w", imp.ID, imp.TermCode, classify(err))
	}

	if _, err := tx.Exec(ctx, importInsertSQL, importInsertArgs(imp, payload)...); err != nil {
		return fmt.Errorf("commit import %s: insert batch: %w", imp.ID, classify(err))
	}

	if imp.Mode == ModeReplace {
		if _, err := tx.Exec(ctx, `
UPDATE schedule_entries
SET superseded_by = $1
WHERE term_code = $2 AND superseded_by IS NULL`, imp.ID, imp.TermCode); err != nil {
			return fmt.Errorf("commit import %s: supersede entries: %w", imp.ID, classify(err))
		}
		if _, err := tx.Exec(ctx, `
UPDATE sessions
SET status = 'canceled', updated_at = now()
WHERE status = 'planned'
  AND starts_at > now()
  AND entry_id IN (SELECT id FROM schedule_entries WHERE superseded_by = $1)`, imp.ID); err != nil {
			return fmt.Errorf("commit import %s: cancel superseded sessions: %w", imp.ID, classify(err))
		}
	}

	for index := range entries {
		entry := entries[index]
		termCode := entry.TermCode
		if termCode == "" {
			termCode = imp.TermCode
		}
		if _, err := tx.Exec(ctx, entryInsertSQL, entry.ID, imp.ID, termCode, entry.CourseCode,
			entry.CourseName, entry.TeacherUsername, entry.TeacherUserID, entry.RoomCode,
			entry.Weekday, entry.PeriodStart, entry.PeriodEnd, entry.Weeks); err != nil {
			return fmt.Errorf("commit import %s: insert entry %s: %w", imp.ID, entry.ID, classify(err))
		}
	}

	for index := range sessions {
		session := sessions[index]
		if _, err := tx.Exec(ctx, importSessionInsertSQL, session.ID, session.EntryID, session.RoomCode,
			session.DeviceID, session.CameraEnum, session.StartsAt, session.EndsAt, session.Status,
			session.Origin, session.StreamID, session.StartedAt, session.StoppedAt, session.RetryCount,
			session.LastError); err != nil {
			return fmt.Errorf("commit import %s: insert session %s: %w", imp.ID, session.ID, classify(err))
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit import %s: %w", imp.ID, classify(err))
	}
	return nil
}

// importInsertArgs builds the batch insert parameters in statement order.
func importInsertArgs(imp domain.Import, payload []byte) []any {
	return []any{
		imp.ID, imp.TermCode, imp.Mode, imp.Filename, imp.SHA256,
		imp.RowCount, imp.OKCount, imp.ErrorCount, payload, imp.Status, imp.ImportedBy,
	}
}

// marshalImportErrors encodes the per-row errors for the JSONB column. An
// empty list is stored as [] rather than null.
func marshalImportErrors(errs []domain.ImportError) ([]byte, error) {
	if len(errs) == 0 {
		return []byte("[]"), nil
	}
	payload, err := json.Marshal(errs)
	if err != nil {
		return nil, fmt.Errorf("encode import errors: %w", err)
	}
	return payload, nil
}
