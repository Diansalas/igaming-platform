package marketgate

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// lic17 L-4: prefilterByCeiling returns every read error (fail closed, explicit) instead of the unfiltered candidate list.

type pfRow struct {
	id  *uuid.UUID
	err error
}

func (r pfRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(**uuid.UUID)) = r.id
	return nil
}

type pfRows struct {
	pgx.Rows
	vals    []string
	i       int
	scanErr error
	iterErr error
}

func (r *pfRows) Next() bool { r.i++; return r.i <= len(r.vals) }
func (r *pfRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	*(dest[0].(*string)) = r.vals[r.i-1]
	return nil
}
func (r *pfRows) Err() error { return r.iterErr }
func (r *pfRows) Close()     {}

type pfTx struct {
	pgx.Tx
	row   pfRow
	query []func() (pgx.Rows, error) // one per Query call, in order
	n     int
}

func (t *pfTx) QueryRow(context.Context, string, ...any) pgx.Row { return t.row }
func (t *pfTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	t.n++
	return t.query[t.n-1]()
}

func TestLic17_L4_PrefilterByCeiling_ReturnsReadErrors(t *testing.T) {
	boom := errors.New("boom")
	lid := uuid.New()
	ok := func(v ...string) func() (pgx.Rows, error) {
		return func() (pgx.Rows, error) { return &pfRows{vals: v}, nil }
	}
	fail := func() (pgx.Rows, error) { return nil, boom }
	scanFail := func() (pgx.Rows, error) { return &pfRows{vals: []string{"MT"}, scanErr: boom}, nil }
	iterFail := func() (pgx.Rows, error) { return &pfRows{iterErr: boom}, nil }
	in := []string{"MT", "DE", "BR"}
	for _, tc := range []struct {
		name  string
		tx    *pfTx
		want  []string
		isErr bool
	}{
		{"tenant read fails", &pfTx{row: pfRow{err: boom}}, nil, true},
		{"tenant row not visible", &pfTx{row: pfRow{err: pgx.ErrNoRows}}, nil, true},
		{"no licence: unchanged (the resolver decides)", &pfTx{row: pfRow{}}, in, false},
		{"ceiling query fails", &pfTx{row: pfRow{id: &lid}, query: []func() (pgx.Rows, error){fail}}, nil, true},
		{"ceiling scan fails", &pfTx{row: pfRow{id: &lid}, query: []func() (pgx.Rows, error){scanFail}}, nil, true},
		{"ceiling iteration fails", &pfTx{row: pfRow{id: &lid}, query: []func() (pgx.Rows, error){iterFail}}, nil, true},
		{"basis query fails", &pfTx{row: pfRow{id: &lid}, query: []func() (pgx.Rows, error){ok("MT"), fail}}, nil, true},
		{"basis scan fails", &pfTx{row: pfRow{id: &lid}, query: []func() (pgx.Rows, error){ok("MT"), scanFail}}, nil, true},
		{"basis iteration fails", &pfTx{row: pfRow{id: &lid}, query: []func() (pgx.Rows, error){ok("MT"), iterFail}}, nil, true},
		{"filters by ceiling and basis", &pfTx{row: pfRow{id: &lid}, query: []func() (pgx.Rows, error){ok("MT"), ok("BR")}}, []string{"MT", "BR"}, false},
	} {
		got, err := prefilterByCeiling(context.Background(), tc.tx, uuid.New(), time.Now(), slices.Clone(in))
		if tc.isErr {
			if err == nil || got != nil {
				t.Fatalf("%s: must return the error and no list, got %v / %v", tc.name, got, err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("%s: got %v / %v, want %v", tc.name, got, err, tc.want)
		}
	}
}
