package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Sentinel errors returned by every store method. They are errors.Is
// compatible and wrap the underlying driver error when one is the cause.
var (
	// ErrNotFound reports a missing row.
	ErrNotFound = errors.New("not found")
	// ErrConflict reports a uniqueness, check or state conflict.
	ErrConflict = errors.New("conflict")
	// ErrValidation reports a value rejected before it reached PostgreSQL.
	ErrValidation = errors.New("validation")
)

// PostgreSQL SQLSTATE codes handled by classify.
const (
	sqlstateUniqueViolation     = "23505"
	sqlstateForeignKeyViolation = "23503"
	sqlstateCheckViolation      = "23514"
)

// isUniqueViolation reports whether err is a PostgreSQL unique violation
// (SQLSTATE 23505), including one wrapped by classify.
func isUniqueViolation(err error) bool {
	return pgErrorCode(err) == sqlstateUniqueViolation
}

// isForeignKeyViolation reports whether err is a PostgreSQL foreign key
// violation (SQLSTATE 23503), including one wrapped by classify.
func isForeignKeyViolation(err error) bool {
	return pgErrorCode(err) == sqlstateForeignKeyViolation
}

// isCheckViolation reports whether err is a PostgreSQL check constraint
// violation (SQLSTATE 23514), including one wrapped by classify.
func isCheckViolation(err error) bool {
	return pgErrorCode(err) == sqlstateCheckViolation
}

// pgErrorCode returns the SQLSTATE of err, or "" when it is not a PostgreSQL
// error.
func pgErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// withSentinel wraps err with a sentinel keeping both errors.Is targets.
func withSentinel(sentinel, err error) error {
	if err == nil {
		return sentinel
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}

// classify maps driver errors onto the package sentinels: no rows becomes
// ErrNotFound, unique and check violations become ErrConflict, missing foreign
// keys become ErrNotFound. Every other error is returned unchanged.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return withSentinel(ErrNotFound, err)
	}
	switch pgErrorCode(err) {
	case sqlstateUniqueViolation, sqlstateCheckViolation:
		return withSentinel(ErrConflict, err)
	case sqlstateForeignKeyViolation:
		return withSentinel(ErrNotFound, err)
	}
	return err
}

// textOrNil maps a scanned nullable TEXT column to a *string: NULL becomes nil.
func textOrNil(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

// timeOrNil maps a scanned nullable TIMESTAMPTZ column to a *time.Time: NULL
// becomes nil.
func timeOrNil(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	at := value.Time
	return &at
}

// intOrNil maps a scanned nullable INTEGER column to an *int: NULL becomes nil.
func intOrNil(value pgtype.Int4) *int {
	if !value.Valid {
		return nil
	}
	number := int(value.Int32)
	return &number
}
