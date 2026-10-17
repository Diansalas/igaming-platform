package db

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// ServerCursor streams the rows of ONE query through a server-side cursor (DECLARE ... NO SCROLL CURSOR) inside an
// already open transaction (SRF2R-1). The query is planned and executed once and its rows are fetched in batches, so
// a long result is read in linear time instead of being re-scanned and re-sorted by re-issued keyset pages; other
// statements may run on the same transaction between two fetches. The cursor lives until Close or the end of the
// transaction, whichever comes first.
type ServerCursor struct {
	tx     pgx.Tx
	name   string
	closed bool
}

var serverCursorName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// DeclareCursor declares cursor name (a fixed identifier chosen by the caller, never user input) over query with
// args, inside tx.
func DeclareCursor(ctx context.Context, tx pgx.Tx, name, query string, args ...any) (*ServerCursor, error) {
	if !serverCursorName.MatchString(name) {
		return nil, fmt.Errorf("db: invalid server cursor name %q", name)
	}
	if _, err := tx.Exec(ctx, `DECLARE `+name+` NO SCROLL CURSOR FOR `+query, args...); err != nil {
		return nil, fmt.Errorf("db: declare cursor %s: %w", name, err)
	}
	return &ServerCursor{tx: tx, name: name}, nil
}

// NextBatch returns the next (at most) n rows. The caller must Close the returned rows before running another statement.
func (c *ServerCursor) NextBatch(ctx context.Context, n int) (pgx.Rows, error) {
	if n <= 0 {
		return nil, fmt.Errorf("db: fetch of %d rows", n)
	}
	return c.tx.Query(ctx, `FETCH FORWARD `+strconv.Itoa(n)+` FROM `+c.name)
}

// Close closes the cursor (idempotent). An error from an already aborted transaction is returned but harmless: the
// cursor ends with the transaction.
func (c *ServerCursor) Close(ctx context.Context) error {
	if c == nil || c.closed {
		return nil
	}
	c.closed = true
	_, err := c.tx.Exec(ctx, `CLOSE `+c.name)
	return err
}
