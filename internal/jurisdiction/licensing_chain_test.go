package jurisdiction

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type chainRow struct {
	vals []any
	err  error
}

func (r chainRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		switch p := d.(type) {
		case **uuid.UUID:
			*p, _ = r.vals[i].(*uuid.UUID)
		case *uuid.UUID:
			*p = r.vals[i].(uuid.UUID)
		case *string:
			*p = r.vals[i].(string)
		}
	}
	return nil
}

type chainQuerier struct{ rows []chainRow }

func (q *chainQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unused")
}
func (q *chainQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	r := q.rows[0]
	q.rows = q.rows[1:]
	return r
}

func TestResolveLicensingChain_TriState(t *testing.T) {
	lic, jur := uuid.New(), uuid.New()
	cases := []struct {
		name   string
		rows   []chainRow
		want   LicensingOutcome
		reason LicensingInvalidReason
		fails  bool
	}{
		{"resolved", []chainRow{{vals: []any{&lic}}, {vals: []any{jur, "KM-ANJ"}}}, LicensingResolved, "", false},
		{"no licence", []chainRow{{vals: []any{(*uuid.UUID)(nil)}}}, LicensingNoLicence, "", false},
		{"tenant not visible", []chainRow{{err: pgx.ErrNoRows}}, LicensingInvalid, LicensingInvalidTenantNotVisible, false},
		{"licence row missing", []chainRow{{vals: []any{&lic}}, {err: pgx.ErrNoRows}}, LicensingInvalid, LicensingInvalidLicenceNotFound, false},
		{"tenant read error", []chainRow{{err: errors.New("boom")}}, "", "", true},
		{"licence read error", []chainRow{{vals: []any{&lic}}, {err: errors.New("boom")}}, "", "", true},
	}
	for _, c := range cases {
		got, err := ResolveLicensingChain(context.Background(), &chainQuerier{rows: c.rows}, uuid.New())
		if c.fails {
			if err == nil {
				t.Fatalf("%s: a read failure must be an error, never a state", c.name)
			}
			continue
		}
		if err != nil || got.Outcome != c.want || got.InvalidReason != c.reason {
			t.Fatalf("%s: got %+v err=%v", c.name, got, err)
		}
		if c.want == LicensingResolved && (got.JurisdictionID != jur || got.JurisdictionCode != "KM-ANJ" || got.LicenceID != lic) {
			t.Fatalf("%s: resolved fields wrong: %+v", c.name, got)
		}
		if c.want != LicensingResolved && (got.JurisdictionID != uuid.Nil || got.JurisdictionCode != "") {
			t.Fatalf("%s: a non-resolved chain must carry no jurisdiction: %+v", c.name, got)
		}
	}
}

func TestResolveLicensingChain_NilTenantIsInvalidInput(t *testing.T) {
	if _, err := ResolveLicensingChain(context.Background(), &chainQuerier{}, uuid.Nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("got %v", err)
	}
}
