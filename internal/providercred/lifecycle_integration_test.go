//go:build integration

// Filing, approval and consume (security review §1.6 "Filing and
// approval" and "Consume"), through the real service and directly
// against the triggers, as the runtime role.
package providercred

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// rawRequestInsert files a request by direct SQL in platform scope as
// principal (bypassing the service's own checks), returning its id and
// content hash.
func (f *fx) rawRequestInsert(principal uuid.UUID, s spec, ref, fingerprint string) (uuid.UUID, string, error) {
	var id uuid.UUID
	var hash string
	err := f.rt.WithPlatformAdmin(context.Background(), principal, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO provider_credential_change_requests
			(target_tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint, vendor_account_id,
			 not_before, not_after, predecessor_handle_id, predecessor_disposition, predecessor_not_after,
			 reason_code, requested_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id, content_hash`,
			s.tenant, s.domain, s.provider, s.purpose, s.keyID, ref, fingerprint, s.vendorAcct,
			s.notBefore, s.notAfter, s.predecessor, s.disposition, s.predNotAfter, s.reason, principal).Scan(&id, &hash)
	})
	return id, hash, err
}

func randomFingerprint(t testing.TB) string {
	return "fp1:" + hexOf(randBytes(t, 32))
}

func TestPCConsume_ApprovedInsertSucceedsAndMarksApplied(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	r, _ := f.file(f.spec(tenant, "acme", "k1"))
	if r.State != "pending" || !contentHashPattern.MatchString(r.ContentHash) {
		t.Fatalf("filed request: state=%s hash=%q", r.State, r.ContentHash)
	}
	f.approve(r, f.approver)
	h, err := f.apply(r)
	if err != nil {
		t.Fatalf("apply: %v (%s)", err, ClassOf(err))
	}
	if h.Status != "active" || h.ActivationRequestID != r.ID || h.TenantID != tenant {
		t.Fatalf("handle: %+v", h)
	}
	var got Request
	if err := f.rt.WithPlatformAdmin(context.Background(), f.requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = GetRequest(ctx, tx, tenant, r.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.State != "applied" || got.AppliedHandleID == nil || *got.AppliedHandleID != h.ID || got.AppliedAt == nil ||
		got.AppliedBy == nil || *got.AppliedBy != f.requester {
		t.Fatalf("request after apply: %+v", got)
	}
}

func TestPCConsume_NoApprovalRefused(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	if _, err := f.apply(r); KindOf(err) != KindActivationRejected || ClassOf(err) != "PC006" {
		t.Fatalf("apply without approval: %v (%s)", err, ClassOf(err))
	}
}

func TestPCConsume_AnyRejectBlocks(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	f.approve(r, f.approver)
	if _, err := f.decide(r, f.approver2, "reject", r.ContentHash, "wrong_vendor_account"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if _, err := f.apply(r); KindOf(err) != KindActivationRejected || ClassOf(err) != "PC007" {
		t.Fatalf("apply after a reject: %v (%s)", err, ClassOf(err))
	}
}

func TestPCConsume_SingleUse(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	f.approve(r, f.approver)
	if _, err := f.apply(r); err != nil {
		t.Fatal(err)
	}
	// The service refuses a non-pending request...
	if _, err := f.apply(r); KindOf(err) != KindActivationRejected {
		t.Fatalf("second apply: %v", err)
	}
	// ...and so does the DB, for a direct insert re-using the request.
	err := f.rt.WithTenant(context.Background(), r.TargetTenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO provider_credential_handles
			(tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint, status, not_before, activation_request_id, created_by)
			VALUES ($1,$2,$3,$4,'k-other',$5,$6,'active',$7,$8,$9)`,
			r.TargetTenantID, r.Domain, r.ProviderID, r.Purpose, r.SecretRef, randomFingerprint(t), r.NotBefore, r.ID, f.requester)
		return err
	})
	if err == nil {
		t.Fatal("a second handle consuming the same request must be refused")
	}
}

func TestPCConsume_ConcurrentApplyExactlyOneWins(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	f.approve(r, f.approver)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = f.apply(r)
		}(i)
	}
	close(start)
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case KindOf(err) != KindActivationRejected:
			t.Fatalf("a losing apply must be activation_rejected, got %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one concurrent apply must win, got %d", wins)
	}
	var handles int
	if err := f.rt.WithTenant(context.Background(), r.TargetTenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM provider_credential_handles WHERE tenant_id = $1`, r.TargetTenantID).Scan(&handles)
	}); err != nil || handles != 1 {
		t.Fatalf("handles = %d, err = %v", handles, err)
	}
}

// directHandleInsert inserts a handle row for request r with the given
// overrides, in tenant scope, as the runtime role (the consume trigger's
// input).
func (f *fx) directHandleInsert(tenantID uuid.UUID, r Request, mutate func(*Handle)) error {
	h := Handle{
		TenantID: r.TargetTenantID, Domain: r.Domain, ProviderID: r.ProviderID, Purpose: r.Purpose, KeyID: r.KeyID,
		SecretRef: r.SecretRef, Fingerprint: r.Fingerprint, VendorAccountID: r.VendorAccountID,
		NotBefore: r.NotBefore, NotAfter: r.NotAfter, ActivationRequestID: r.ID, CreatedBy: f.requester,
	}
	if mutate != nil {
		mutate(&h)
	}
	return f.rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO provider_credential_handles
			(tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint, vendor_account_id,
			 status, not_before, not_after, activation_request_id, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'active',$9,$10,$11,$12)`,
			h.TenantID, h.Domain, h.ProviderID, h.Purpose, h.KeyID, h.SecretRef, h.Fingerprint, h.VendorAccountID,
			h.NotBefore, h.NotAfter, h.ActivationRequestID, h.CreatedBy)
		return err
	})
}

// TestPCConsume_ContentBinding: one subtest per bound column (including
// NULL<->value for vendor_account_id and not_after, and the tenant). Each
// mutated insert is refused; the unmutated one then succeeds, proving the
// refusals were for the mutation alone.
func TestPCConsume_ContentBinding(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	other := f.tenant()
	s := f.spec(tenant, "acme", "k1")
	s.vendorAcct = ptr("merchant-1")
	s.notAfter = ptr(time.Now().Add(48 * time.Hour).Truncate(time.Microsecond))
	r, _ := f.file(s)
	f.approve(r, f.approver)

	otherRef, _ := f.put(s)
	cases := map[string]func(*Handle){
		"domain":                        func(h *Handle) { h.Domain = "payments"; h.SecretRef = memRef(tenant, "payments", "acme", "x") },
		"provider_id":                   func(h *Handle) { h.ProviderID = "acme2"; h.SecretRef = memRef(tenant, "casino", "acme2", "x") },
		"purpose":                       func(h *Handle) { h.Purpose = PurposeOutboundAPI },
		"key_id":                        func(h *Handle) { h.KeyID = "k2" },
		"secret_ref":                    func(h *Handle) { h.SecretRef = otherRef },
		"fingerprint":                   func(h *Handle) { h.Fingerprint = randomFingerprint(t) },
		"vendor_account_id value":       func(h *Handle) { h.VendorAccountID = ptr("merchant-2") },
		"vendor_account_id value->NULL": func(h *Handle) { h.VendorAccountID = nil },
		"not_before":                    func(h *Handle) { h.NotBefore = h.NotBefore.Add(-time.Second) },
		"not_after value":               func(h *Handle) { h.NotAfter = ptr(h.NotAfter.Add(-time.Second)) },
		"not_after value->NULL":         func(h *Handle) { h.NotAfter = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := f.directHandleInsert(tenant, r, mutate); err == nil {
				t.Fatalf("a handle differing in %s must be refused", name)
			}
		})
	}
	t.Run("tenant", func(t *testing.T) {
		// Another tenant cannot see (so cannot consume) this request.
		err := f.directHandleInsert(other, r, func(h *Handle) {
			h.TenantID = other
			h.SecretRef = memRef(other, "casino", "acme", "x")
		})
		if err == nil {
			t.Fatal("another tenant's insert must be refused")
		}
	})
	if err := f.directHandleInsert(tenant, r, nil); err != nil {
		t.Fatalf("the exact approved content must be accepted: %v", err)
	}

	// NULL -> value, on a request whose optional columns are NULL.
	s2 := f.spec(tenant, "acme", "k9")
	r2, _ := f.file(s2)
	f.approve(r2, f.approver)
	for name, mutate := range map[string]func(*Handle){
		"vendor_account_id NULL->value": func(h *Handle) { h.VendorAccountID = ptr("merchant-1") },
		"not_after NULL->value":         func(h *Handle) { h.NotAfter = ptr(time.Now().Add(time.Hour)) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := f.directHandleInsert(tenant, r2, mutate); err == nil {
				t.Fatalf("a handle differing in %s must be refused", name)
			}
		})
	}
}

func TestPCConsume_HashIndependentOfSessionTimeZone(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	var hashes []string
	for _, tz := range []string{"UTC", "America/Sao_Paulo", "Asia/Kolkata", "Pacific/Chatham"} {
		var h string
		err := f.rt.WithPlatformAdmin(context.Background(), f.requester, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('TimeZone', $1, true)`, tz); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT provider_credential_content_hash(target_tenant_id, domain, provider_id, purpose, key_id,
				secret_ref, fingerprint, vendor_account_id, not_before, not_after, predecessor_handle_id,
				predecessor_disposition, predecessor_not_after) FROM provider_credential_change_requests WHERE id = $1`, r.ID).Scan(&h)
		})
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}
	for _, h := range hashes {
		if h != r.ContentHash {
			t.Fatalf("hash depends on the session TimeZone: stored %s, got %v", r.ContentHash, hashes)
		}
	}
}

// TestPCConsume_PredecessorDisposition: verify_only with the wrong
// not_after; a predecessor not demoted in this transaction; revoked passes;
// none with an existing active row is refused.
func TestPCConsume_PredecessorDisposition(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	h1, _ := f.register(f.spec(tenant, "acme", "k1"))

	t.Run("none with an existing active row", func(t *testing.T) {
		r, _ := f.file(f.spec(tenant, "acme", "k2"))
		f.approve(r, f.approver)
		if _, err := f.apply(r); KindOf(err) != KindActivationRejected {
			t.Fatalf("got %v", err)
		}
	})

	pna := time.Now().Add(72 * time.Hour).Truncate(time.Microsecond)
	s := f.spec(tenant, "acme", "k3")
	s.predecessor, s.disposition, s.predNotAfter, s.reason = &h1.ID, DispositionVerifyOnly, &pna, RequestReasonScheduledRotation
	r, _ := f.file(s)
	f.approve(r, f.approver)

	t.Run("verify_only with the wrong not_after", func(t *testing.T) {
		err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE provider_credential_handles SET status = 'verify_only', not_after = $2 WHERE id = $1`,
				h1.ID, pna.Add(-time.Hour)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO provider_credential_handles
				(tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint, status, not_before, activation_request_id, created_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7,'active',$8,$9,$10)`,
				tenant, r.Domain, r.ProviderID, r.Purpose, r.KeyID, r.SecretRef, r.Fingerprint, r.NotBefore, r.ID, f.requester)
			return err
		})
		if pgCode(err) != "PC008" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("predecessor not demoted in this transaction", func(t *testing.T) {
		// Demote in one transaction...
		if err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE provider_credential_handles SET status = 'verify_only', not_after = $2 WHERE id = $1`, h1.ID, pna)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// ...insert in another: refused.
		err := f.directHandleInsert(tenant, r, nil)
		if pgCode(err) != "PC008" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("revoked passes", func(t *testing.T) {
		f.revoke(tenant, h1.ID)
		s := f.spec(tenant, "acme", "k4")
		s.predecessor, s.disposition, s.reason = &h1.ID, DispositionRevoked, RequestReasonCompromiseReplacement
		r4, _ := f.file(s)
		f.approve(r4, f.approver)
		if _, err := f.apply(r4); err != nil {
			t.Fatalf("revoked predecessor must pass: %v (%s)", err, ClassOf(err))
		}
	})
}

// --- filing and approval --------------------------------------------------

func TestPCRequest_RequesterEligibility(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	tenantAdmin := f.tenantStaff(tenant, "tenant_admin")
	for name, principal := range map[string]uuid.UUID{
		"unresolvable":  uuid.New(),
		"tenant-scoped": tenantAdmin,
		"unlinked":      f.unlinked,
		"suspended":     f.suspended,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := f.rawRequestInsert(principal, f.spec(tenant, "acme", "k1"),
				memRef(tenant, "casino", "acme", "n"), randomFingerprint(t))
			if err == nil {
				t.Fatal("an ineligible requester must be refused")
			}
			if name != "tenant-scoped" && pgCode(err) != "PC030" {
				// A tenant-scoped principal is refused at the latest by the
				// platform_admin_scope WITH CHECK (it resolves in no platform
				// row); every other case is PC030.
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestPCRequest_ServerFieldsForced(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	var reqAt time.Time
	var state, hash string
	var appliedAt *time.Time
	err := f.rt.WithPlatformAdmin(context.Background(), f.requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO provider_credential_change_requests
			(target_tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint, not_before,
			 predecessor_disposition, reason_code, requested_by_principal_id, requested_at, state, content_hash, applied_at)
			VALUES ($1,'casino','acme','webhook_verify','k1',$2,$3,now(),'none','initial_registration',$4,
			        '2999-01-01','applied',$5,'2999-01-01') RETURNING requested_at, state, content_hash, applied_at`,
			tenant, memRef(tenant, "casino", "acme", "n"), randomFingerprint(t), f.requester, repeat("0", 64)).
			Scan(&reqAt, &state, &hash, &appliedAt)
	})
	if err != nil {
		t.Fatal(err)
	}
	if reqAt.Year() == 2999 || state != "pending" || hash == repeat("0", 64) || appliedAt != nil {
		t.Fatalf("client values must be ignored: requested_at=%s state=%s hash=%s applied_at=%v", reqAt, state, hash, appliedAt)
	}
}

func TestPCRequest_ImmutableAfterInsert(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	r, _ := f.file(f.spec(tenant, "acme", "k1"))
	for col, val := range map[string]string{
		"target_tenant_id":          "'" + f.tenant().String() + "'",
		"domain":                    "'payments'",
		"provider_id":               "'acme2'",
		"purpose":                   "'outbound_api'",
		"key_id":                    "'k2'",
		"secret_ref":                "'" + memRef(tenant, "casino", "acme", "other") + "'",
		"fingerprint":               "'" + randomFingerprint(t) + "'",
		"vendor_account_id":         "'m'",
		"not_before":                "now() - interval '1 hour'",
		"not_after":                 "now() + interval '1 day'",
		"predecessor_disposition":   "'revoked'",
		"predecessor_not_after":     "now() + interval '1 day'",
		"content_hash":              "'" + repeat("1", 64) + "'",
		"reason_code":               "'vendor_migration'",
		"requested_by_principal_id": "'" + f.approver.String() + "'",
		"requested_at":              "now() - interval '1 day'",
		"state":                     "'applied'",
	} {
		t.Run(col, func(t *testing.T) {
			err := f.rt.WithPlatformAdmin(context.Background(), f.requester, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE provider_credential_change_requests SET `+col+` = `+val+` WHERE id = $1`, r.ID)
				return err
			})
			if err == nil {
				t.Fatalf("updating %s must be refused", col)
			}
		})
	}
	t.Run("DELETE", func(t *testing.T) {
		err := f.rt.WithPlatformAdmin(context.Background(), f.requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM provider_credential_change_requests WHERE id = $1`, r.ID)
			return err
		})
		if err == nil {
			t.Fatal("DELETE must be refused")
		}
	})
	// TRUNCATE is exercised on a scratch database
	// (TestPCMigration_TruncateRefused), never on the shared test DB.
}

func TestPCApproval_SelfApprovalRefused(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	if _, err := f.decide(r, f.requester, "approve", r.ContentHash, ""); KindOf(err) != KindApprovalRejected || ClassOf(err) != "PC041" {
		t.Fatalf("got %v (%s)", err, ClassOf(err))
	}
}

func TestPCApproval_SamePersonSecondAccountRefused(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	if _, err := f.decide(r, f.samePerson, "approve", r.ContentHash, ""); KindOf(err) != KindApprovalRejected || ClassOf(err) != "PC044" {
		t.Fatalf("got %v (%s)", err, ClassOf(err))
	}
}

func TestPCApproval_ApproverEligibility(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	r, _ := f.file(f.spec(tenant, "acme", "k1"))
	tenantAdmin := f.tenantStaff(tenant, "tenant_admin")
	for name, principal := range map[string]uuid.UUID{
		"unresolvable":  uuid.New(),
		"tenant-scoped": tenantAdmin,
		"unlinked":      f.unlinked,
		"suspended":     f.suspended,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.decide(r, principal, "approve", r.ContentHash, ""); KindOf(err) != KindApprovalRejected || ClassOf(err) != "PC042" {
				t.Fatalf("got %v (%s)", err, ClassOf(err))
			}
		})
	}
}

func TestPCApproval_ContentHashMismatchRefused(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	if _, err := f.decide(r, f.approver, "approve", repeat("a", 64), ""); KindOf(err) != KindApprovalRejected || ClassOf(err) != "PC046" {
		t.Fatalf("got %v (%s)", err, ClassOf(err))
	}
}

func TestPCApproval_NotPendingRefused(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	f.approve(r, f.approver)
	if _, err := f.apply(r); err != nil {
		t.Fatal(err)
	}
	if _, err := f.decide(r, f.approver2, "approve", r.ContentHash, ""); KindOf(err) != KindApprovalRejected || ClassOf(err) != "PC045" {
		t.Fatalf("got %v (%s)", err, ClassOf(err))
	}
}

func TestPCApproval_DuplicateApproverRefused(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	f.approve(r, f.approver)
	if _, err := f.decide(r, f.approver, "approve", r.ContentHash, ""); KindOf(err) != KindApprovalRejected || ClassOf(err) != "unique_violation" {
		t.Fatalf("got %v (%s)", err, ClassOf(err))
	}
}

func TestPCApproval_DecidedAtClientValueIgnored(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	var decided time.Time
	err := f.rt.WithPlatformAdmin(context.Background(), f.approver, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO provider_credential_change_approvals
			(request_id, approver_principal_id, decision, content_hash, decided_at)
			VALUES ($1, $2, 'approve', $3, now() + interval '30 days') RETURNING decided_at`,
			r.ID, f.approver, r.ContentHash).Scan(&decided)
	})
	if err != nil {
		t.Fatal(err)
	}
	if decided.After(time.Now().Add(time.Minute)) {
		t.Fatalf("a future decided_at must be stored as now(), got %s", decided)
	}
}

func TestPCApproval_ApproverMustBeSessionPrincipal(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	// A platform session of the REQUESTER recording an approval attributed
	// to someone else is refused (PC031).
	err := f.rt.WithPlatformAdmin(context.Background(), f.requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO provider_credential_change_approvals
			(request_id, approver_principal_id, decision, content_hash) VALUES ($1, $2, 'approve', $3)`,
			r.ID, f.approver, r.ContentHash)
		return err
	})
	if pgCode(err) != "PC031" {
		t.Fatalf("got %v", err)
	}
}

func TestPCApproval_Immutable(t *testing.T) {
	f := newFx(t)
	r, _ := f.file(f.spec(f.tenant(), "acme", "k1"))
	ap, err := f.decide(r, f.approver, "approve", r.ContentHash, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{
		"UPDATE": `UPDATE provider_credential_change_approvals SET decision = 'reject', reason_code = 'x' WHERE id = $1`,
		"DELETE": `DELETE FROM provider_credential_change_approvals WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			err := f.rt.WithPlatformAdmin(context.Background(), f.approver, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql, ap.ID)
				return err
			})
			if err == nil {
				t.Fatalf("%s must be refused", name)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
