//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Rows sharing one created_at are paged by the id tiebreak: no duplicate, no gap (the keyset is
// (created_at, id), not created_at alone). The tie is forced on the scratch database only.
func TestStaffReadR1_PaginationTies(t *testing.T) {
	w := newSRWorld(t)
	ctx := context.Background()
	if err := w.a.pool.WithTenant(ctx, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE payment_attempts DISABLE TRIGGER USER`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET created_at = (SELECT created_at FROM payment_attempts WHERE id = $1) WHERE id = ANY($2)`,
			w.depA[0], []uuid.UUID{w.depA[1], w.depA[2], w.ambAtt}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE payment_attempts ENABLE TRIGGER USER`)
		return err
	}); err != nil {
		t.Skipf("cannot force a created_at tie on this scratch database: %v", err)
	}
	var want []string
	if err := w.a.pool.WithTenant(ctx, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text FROM payment_attempts WHERE tenant_id = $1 ORDER BY created_at DESC, id DESC`, w.tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			want = append(want, s)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	var got []string
	cur := ""
	for page := 0; page < 20; page++ {
		q := "limit=1"
		if cur != "" {
			q += "&cursor=" + url.QueryEscape(cur)
		}
		res := w.a.do("GET", w.attemptsURL(w.tenant, q), w.tok(w.f1), nil)
		if res.status != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, res.status, res.body)
		}
		pg := decodePage(t, res)
		for _, it := range pg.Items {
			got = append(got, it.ID)
		}
		if pg.NextCursor == nil {
			break
		}
		cur = *pg.NextCursor
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("paging through a created_at tie = %v, want %v", got, want)
	}
}

// A cancelled K3 resolution is not "open": the attempt is reported without it and admits its
// kinds again.
func TestStaffReadR1_CancelledResolutionIsNotOpen(t *testing.T) {
	w := newSRWorld(t)
	code, body, dto := w.request(w.f1, w.depA[0], nil)
	if code != http.StatusCreated {
		t.Fatalf("M1 request: %d %s", code, body)
	}
	one := func() paymentAttemptDTO {
		return decodeAttempt(t, w.a.do("GET", w.attemptsURL(w.tenant, "")+"/"+w.depA[0].String(), w.tok(w.comp), nil))
	}
	if d := one(); d.OpenResolutionID == nil || *d.OpenResolutionID != dto.ID {
		t.Fatalf("pending resolution not reported: %+v", d)
	}
	if res := w.a.do("POST", w.base()+"/"+dto.ID+"/cancel", w.tok(w.f1), map[string]any{}); res.status != http.StatusOK {
		t.Fatalf("cancel: %d %s", res.status, res.body)
	}
	d := one()
	if d.OpenResolutionID != nil || len(d.AdmissibleKinds) != 1 || d.AdmissibleKinds[0] != "m1_deposit_evidence" {
		t.Errorf("after cancel: open_resolution_id=%v kinds=%v, want none and [m1_deposit_evidence]", d.OpenResolutionID, d.AdmissibleKinds)
	}
}
