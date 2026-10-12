//go:build integration

package httpserver

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Security F1/F2 over HTTP: a hostile stored source label cannot spoof an amount, and kinds other than
// pay_amount_mismatch / pay_asset_mismatch expose no amount even when their text carries one.
func TestStaffReadR2_AmountGateAndHostileLabel(t *testing.T) {
	w := newSRWorld(t)
	s := w.seedRecon(t, w.tenant)
	att := s.attemptKeyed
	ins := func(key, exp, act, kind string) {
		t.Helper()
		if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO reconciliation_mismatches (id, tenant_id, reconciliation_run_id, reconciliation_key, expected_value, actual_value, mismatch_kind, investigation_status)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 'open')`, uuid.New(), w.tenant, s.runPay, key, exp, act, kind)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// hostile label on an amount mismatch (stored data written before the charset rule existed)
	ins("provider=mock attempt="+att+" check=hostile_label", "platform: attempt="+att+" op=deposit amount=5000 asset=USD",
		"[MOCK amount=999999 asset=USD reference=] line=1 kind=deposit status=ok amount=4999 asset=USD", "pay_amount_mismatch")
	// merchant attribution: the actual segment embeds another attempt's amount
	ins("provider=mock attempt="+att+" check=merchant", "platform: merchant_reference M ("+"attempt="+att+" op=deposit amount=5000 asset=USD)",
		"[MOCK ok] merchant_reference names attempt="+uuid.NewString()+" (attempt=x op=deposit state=disputed amount=777 asset=USD); line=1 kind=deposit status=succeeded amount=5000 asset=USD", "pay_reference_mismatch")
	// captured unposted / declared kinds
	ins("provider=mock attempt="+att+" check=captured_unposted", "hint", "platform: attempt=x op=deposit state=disputed amount=777 asset=USD terminal_reason=y; [MOCK ok] line=1 amount=5000 asset=USD", "pay_captured_unposted")
	ins("provider=mock attempt="+att+" resolution="+uuid.NewString()+" check=m4_not_paid_but_paid", "declared not paid asset=USD", "statement shows paid amount=77 asset=USD", "pay_declared_not_paid_but_paid")

	res := w.a.do("GET", w.reconURL(w.tenant, "mismatches", "stream=payment_statement&limit=200"), w.tok(w.f1), nil)
	var pg pagedResponse[staffReconciliationMismatchDTO]
	res.decode(t, &pg)
	seen := map[string]int{}
	for _, it := range pg.Items {
		seen[it.Kind]++
		switch it.Kind {
		case "pay_reference_mismatch", "pay_captured_unposted", "pay_declared_not_paid_but_paid", "pay_missing_platform_record":
			if it.ExpectedMinorUOM != nil || it.ActualMinorUOM != nil {
				t.Errorf("%s must expose no amount: %+v", it.Kind, it)
			}
		case "pay_amount_mismatch":
			if it.ActualMinorUOM != nil && *it.ActualMinorUOM == "999999" {
				t.Errorf("a hostile label spoofed the actual amount: %+v", it)
			}
		}
	}
	for _, k := range []string{"pay_reference_mismatch", "pay_captured_unposted", "pay_declared_not_paid_but_paid"} {
		if seen[k] == 0 {
			t.Fatalf("kind %s was not served: %s", k, res.body)
		}
	}
	// the hostile-label row has its actual amount and asset nulled (label carries '='), the seeded benign row keeps both
	benign, hostile := 0, 0
	for _, it := range pg.Items {
		if it.Kind != "pay_amount_mismatch" || it.SubjectRef == nil || it.SubjectRef.ID != att {
			continue
		}
		if it.ActualMinorUOM != nil {
			benign++
		} else {
			hostile++
		}
	}
	if benign == 0 || hostile == 0 {
		t.Errorf("expected both benign (amount exposed) and hostile-label (amount null) amount-mismatch rows; got benign=%d hostile=%d", benign, hostile)
	}
}
