//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Security C1: a platform principal is never placed in app.principal_id, so a platform_admin WITHOUT
// any K1 grant reading an attempt that has a pending resolution gets open_resolution_id = null (the
// tenant callers still see it). The statements a platform read runs are checked at the database too.
func TestStaffReadR1_PlatformCallerNeverSeesOpenResolution(t *testing.T) {
	w := newSRWorld(t)
	code, body, dto := w.request(w.f1, w.depA[0], nil)
	if code != http.StatusCreated {
		t.Fatalf("M1 request: %d %s", code, body)
	}
	url := w.attemptsURL(w.tenant, "") + "/" + w.depA[0].String()
	// tenant callers: visible.
	for _, tok := range []string{w.tok(w.f1), w.tok(w.comp)} {
		if d := decodeAttempt(t, w.a.do("GET", url, tok, nil)); d.OpenResolutionID == nil || *d.OpenResolutionID != dto.ID {
			t.Fatalf("tenant caller lost the open resolution: %+v", d)
		}
	}
	// platform callers, with and without a K1 grant: null on detail and list.
	for name, tok := range map[string]string{"no grant": w.tok(w.actingNoGrant), "pa": w.tok(w.pa), "granted acting": w.tok(w.acting)} {
		res := w.a.do("GET", url, tok, nil)
		d := decodeAttempt(t, res)
		if d.OpenResolutionID != nil || strings.Contains(string(res.body), dto.ID) {
			t.Errorf("platform caller (%s) learned the open resolution: %s", name, res.body)
		}
		list := w.a.do("GET", w.attemptsURL(w.tenant, "limit=200"), tok, nil)
		if strings.Contains(string(list.body), dto.ID) {
			t.Errorf("platform caller (%s) list carries the resolution id", name)
		}
	}
	// the session a platform read opens carries no principal: probe the GUC through the same helper.
	var principal string
	if err := w.a.pool.WithTenantReadOnly(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(current_setting('app.principal_id', true), '')`).Scan(&principal)
	}); err != nil || principal != "" {
		t.Errorf("a plain tenant read-only session carries principal %q (err %v)", principal, err)
	}
}

// Security C2 / ledger-finance F1: the subject_id filter and the subject_ref agree, and neither is
// fooled by a reference that contains spaces and injected tokens.
func TestStaffReadR2_InjectedTokensNeverMatchOrSurface(t *testing.T) {
	w := newSRWorld(t)
	s := w.seedRecon(t, w.tenant)
	inj, injBet := uuid.NewString(), uuid.NewString()
	ref := "my ref"
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		ins := func(run uuid.UUID, key, exp, act, kind string) error {
			_, err := tx.Exec(ctx, `INSERT INTO reconciliation_mismatches (id, tenant_id, reconciliation_run_id, reconciliation_key, expected_value, actual_value, mismatch_kind, investigation_status)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 'open')`, uuid.New(), w.tenant, run, key, exp, act, kind)
			return err
		}
		// provider reference with spaces and injected subject / bet / asset / amount tokens.
		if err := ins(s.runPay, "provider=mock provider_reference="+ref+" attempt="+inj+" bet="+injBet+" kind=deposit check=resolve",
			"platform: an attempt of this provider with this reference",
			"line=1 kind=deposit status=ok amount=1 amount=2 asset=USD merchant_reference=M asset=XTS", "pay_missing_platform_record"); err != nil {
			return err
		}
		// the real attempt id rendered once in the trusted prefix, plus an injected asset and amount after the reference.
		if err := ins(s.runPay, "provider=mock provider_reference=x kind=deposit attempt="+inj+" check=amount",
			"platform: attempt="+s.attemptKeyed+" op=deposit amount=10 asset=USD provider_reference=r amount=999 asset=EUR",
			"line=1 kind=deposit status=ok amount=11 asset=USD merchant_reference=m", "pay_amount_mismatch"); err != nil {
			return err
		}
		// sportsbook statement diff: free text in the key.
		return ins(s.runSB, "statement: ref bet="+injBet+" wallet="+inj, "ledger: x", "statement: y", "sb_mock_statement_mismatch")
	}); err != nil {
		t.Fatal(err)
	}
	tok := w.tok(w.f1)
	count := func(stream, q string) (int, []staffReconciliationMismatchDTO) {
		res := w.a.do("GET", w.reconURL(w.tenant, "mismatches", "stream="+stream+q), tok, nil)
		if res.status != http.StatusOK {
			t.Fatalf("%s %s: %d %s", stream, q, res.status, res.body)
		}
		var pg pagedResponse[staffReconciliationMismatchDTO]
		res.decode(t, &pg)
		return pg.Total, pg.Items
	}
	// An injected id matches nothing, in any stream.
	for _, stream := range []string{"payment_statement", "sportsbook_settlement", "ledger_vs_projection"} {
		for _, id := range []string{inj, injBet} {
			if n, _ := count(stream, "&subject_id="+id); n != 0 {
				t.Errorf("%s: injected id %s matched %d rows through the subject_id filter", stream, id, n)
			}
		}
	}
	// The real attempt id matches exactly the rows that name it in a trusted position (the expected segment of the
	// amount mismatch, the key of the declared-not-paid row).
	n, items := count("payment_statement", "&subject_id="+s.attemptKeyed)
	if n != 3 {
		t.Errorf("subject_id for the real attempt: %d rows, want 3", n)
	}
	// subject_ref never surfaces an injected id; the injected asset/amount never surface.
	for _, stream := range []string{"payment_statement", "sportsbook_settlement"} {
		_, all := count(stream, "&limit=200")
		for _, it := range all {
			if it.SubjectRef != nil && (it.SubjectRef.ID == inj || it.SubjectRef.ID == injBet) {
				t.Errorf("%s: injected id surfaced as subject_ref: %+v", stream, it)
			}
			if it.AssetCode != nil && *it.AssetCode != "USD" {
				t.Errorf("%s: asset %q surfaced", stream, *it.AssetCode)
			}
		}
	}
	sawHostile := false
	for _, it := range items {
		if it.Kind != "pay_amount_mismatch" || it.ExpectedMinorUOM == nil || it.ActualMinorUOM == nil {
			continue
		}
		// the seeded row is 5000/4999; the hostile-but-valid row has amount=10 once in the trusted
		// prefix (the injected amount=999 sits after provider_reference=) and 11 on the line.
		switch *it.ExpectedMinorUOM + "/" + *it.ActualMinorUOM {
		case "5000/4999":
		case "10/11":
			sawHostile = true
		default:
			t.Errorf("unexpected amounts %s/%s (an injected value surfaced)", *it.ExpectedMinorUOM, *it.ActualMinorUOM)
		}
	}
	if !sawHostile {
		t.Error("the hostile-but-valid row did not yield its trusted amounts 10/11")
	}
	// The missing-record row with duplicated amount= and two asset tokens yields no amount and no asset.
	_, all := count("payment_statement", "&kind=pay_missing_platform_record&limit=200")
	for _, it := range all {
		if it.ActualMinorUOM != nil || it.ExpectedMinorUOM != nil {
			t.Errorf("duplicate amount= must yield null amounts: %+v", it)
		}
	}
}

// Ledger-finance F3: an asset that is not in the asset registry nulls the asset and both amounts.
func TestStaffReadR2_UnknownAssetNullsAmounts(t *testing.T) {
	w := newSRWorld(t)
	s := w.seedRecon(t, w.tenant)
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO reconciliation_mismatches (id, tenant_id, reconciliation_run_id, reconciliation_key, expected_value, actual_value, mismatch_kind, investigation_status)
			VALUES ($1, $2, $3, $4, 'platform: attempt='||$5||' op=deposit amount=10 asset=ZZZ', 'line=1 amount=11 asset=ZZZ', 'pay_amount_mismatch', 'open')`,
			uuid.New(), w.tenant, s.runPay, "provider=mock attempt="+s.attemptKeyed+" check=amount", s.attemptKeyed)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res := w.a.do("GET", w.reconURL(w.tenant, "mismatches", "stream=payment_statement&limit=200"), w.tok(w.f1), nil)
	var pg pagedResponse[staffReconciliationMismatchDTO]
	res.decode(t, &pg)
	found := false
	for _, it := range pg.Items {
		if it.SubjectRef != nil && it.SubjectRef.ID == s.attemptKeyed && it.Kind == "pay_amount_mismatch" && it.AssetCode == nil {
			found = true
			if it.ExpectedMinorUOM != nil || it.ActualMinorUOM != nil || it.Exponent != nil {
				t.Errorf("unknown asset must null amounts and exponent: %+v", it)
			}
		}
	}
	if !found {
		t.Fatalf("the unknown-asset row was not served: %s", res.body)
	}
}
