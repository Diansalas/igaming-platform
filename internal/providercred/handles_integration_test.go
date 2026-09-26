//go:build integration

// Handle rules and transitions (security review §1.6 "Handle rules"; T6,
// T7), plus the secret_ref namespace CHECK on both tables.
package providercred

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPCHandles_InsertRequiresActivationRequest(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	r, _ := f.file(f.spec(tenant, "acme", "k1"))
	f.approve(r, f.approver)
	for name, mutate := range map[string]func(*Handle){
		"no activation request":      func(h *Handle) { h.ActivationRequestID = uuid.Nil },
		"unknown activation request": func(h *Handle) { h.ActivationRequestID = uuid.New() },
		"no created_by":              func(h *Handle) { h.CreatedBy = uuid.Nil },
	} {
		t.Run(name, func(t *testing.T) {
			err := f.directHandleInsertNullable(tenant, r, mutate)
			if err == nil {
				t.Fatalf("%s must be refused", name)
			}
		})
	}
}

// directHandleInsertNullable is directHandleInsert with uuid.Nil mapped to
// SQL NULL for activation_request_id/created_by.
func (f *fx) directHandleInsertNullable(tenantID uuid.UUID, r Request, mutate func(*Handle)) error {
	h := Handle{
		TenantID: r.TargetTenantID, Domain: r.Domain, ProviderID: r.ProviderID, Purpose: r.Purpose, KeyID: r.KeyID,
		SecretRef: r.SecretRef, Fingerprint: r.Fingerprint, NotBefore: r.NotBefore, ActivationRequestID: r.ID, CreatedBy: f.requester,
	}
	mutate(&h)
	nullable := func(u uuid.UUID) *uuid.UUID {
		if u == uuid.Nil {
			return nil
		}
		return &u
	}
	return f.rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO provider_credential_handles
			(tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint, status, not_before, activation_request_id, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'active',$8,$9,$10)`,
			h.TenantID, h.Domain, h.ProviderID, h.Purpose, h.KeyID, h.SecretRef, h.Fingerprint, h.NotBefore,
			nullable(h.ActivationRequestID), nullable(h.CreatedBy))
		return err
	})
}

func TestPCHandles_InsertNonActiveRefused(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	r, _ := f.file(f.spec(tenant, "acme", "k1"))
	f.approve(r, f.approver)
	for _, status := range []string{"verify_only", "revoked"} {
		t.Run(status, func(t *testing.T) {
			err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO provider_credential_handles
					(tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint, status, not_before, not_after,
					 activation_request_id, created_by, revoked_at, revoked_by, revoke_reason)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, now() + interval '1 day', $10, $11,
					        CASE WHEN $8 = 'revoked' THEN now() END, CASE WHEN $8 = 'revoked' THEN $11::uuid END,
					        CASE WHEN $8 = 'revoked' THEN 'misregistration' END)`,
					tenant, r.Domain, r.ProviderID, r.Purpose, r.KeyID, r.SecretRef, r.Fingerprint, status, r.NotBefore, r.ID, f.requester)
				return err
			})
			if pgCode(err) != "PC010" {
				t.Fatalf("a %s insert must be refused with PC010, got %v", status, err)
			}
		})
	}
}

// handleUpdate runs one UPDATE on a handle in tenant scope as the runtime
// role (or as the owner when owner != nil).
func (f *fx) handleUpdate(pool TenantTxRunner, tenant, id uuid.UUID, set string, args ...any) error {
	return pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE provider_credential_handles SET `+set+` WHERE id = $1`, append([]any{id}, args...)...)
		return err
	})
}

func TestPCHandleTransition_Rules(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	rt := f.rt
	newActive := func(t *testing.T) Handle {
		h, _ := f.register(f.spec(tenant, "p-"+uuid.NewString()[:8], "k1"))
		return h
	}
	newVerifyOnly := func(t *testing.T) Handle {
		h := newActive(t)
		if _, err := f.transition(tenant, h.ID, ActionVerifyOnly, ptr(time.Now().Add(48*time.Hour)), TransitionReasonRotation); err != nil {
			t.Fatal(err)
		}
		return h
	}
	newRevoked := func(t *testing.T) Handle {
		h := newActive(t)
		f.revoke(tenant, h.ID)
		return h
	}

	t.Run("backward verify_only -> active refused", func(t *testing.T) {
		h := newVerifyOnly(t)
		if err := f.handleUpdate(rt, tenant, h.ID, `status = 'active'`); pgCode(err) != "PC022" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("revoked -> active refused", func(t *testing.T) {
		h := newRevoked(t)
		if err := f.handleUpdate(rt, tenant, h.ID, `status = 'active', revoked_at = NULL, revoked_by = NULL, revoke_reason = NULL`); pgCode(err) != "PC020" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("anything out of revoked refused", func(t *testing.T) {
		h := newRevoked(t)
		for _, set := range []string{
			`status = 'verify_only', not_after = now() + interval '1 day'`,
			`not_after = now() + interval '1 hour'`,
			`revoke_reason = 'tenant_request'`,
		} {
			if err := f.handleUpdate(rt, tenant, h.ID, set); pgCode(err) != "PC020" {
				t.Fatalf("%s: got %v", set, err)
			}
		}
	})
	t.Run("verify_only on outbound_api refused", func(t *testing.T) {
		s := f.spec(tenant, "p-"+uuid.NewString()[:8], "out")
		s.purpose = PurposeOutboundAPI
		h, _ := f.register(s)
		if _, err := f.transition(tenant, h.ID, ActionVerifyOnly, ptr(time.Now().Add(time.Hour)), TransitionReasonRotation); KindOf(err) != KindTransitionRejected {
			t.Fatalf("got %v (%s)", err, ClassOf(err))
		}
	})
	t.Run("7-day cap: exactly 7d passes", func(t *testing.T) {
		h := newActive(t)
		if err := f.handleUpdate(rt, tenant, h.ID, `status = 'verify_only', not_after = now() + interval '7 days'`); err != nil {
			t.Fatalf("exactly 7 days must pass: %v", err)
		}
	})
	t.Run("7-day cap: 7d+1s refused", func(t *testing.T) {
		h := newActive(t)
		if err := f.handleUpdate(rt, tenant, h.ID, `status = 'verify_only', not_after = now() + interval '7 days 1 second'`); pgCode(err) != "PC024" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("not_after extend refused", func(t *testing.T) {
		h := newVerifyOnly(t)
		if err := f.handleUpdate(rt, tenant, h.ID, `not_after = not_after + interval '1 second'`); pgCode(err) != "PC023" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("not_after clear refused", func(t *testing.T) {
		h := newVerifyOnly(t)
		if err := f.handleUpdate(rt, tenant, h.ID, `not_after = NULL`); pgCode(err) != "PC023" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("not_after in the past refused", func(t *testing.T) {
		h := newActive(t)
		if err := f.handleUpdate(rt, tenant, h.ID, `not_after = now() - interval '1 second'`); pgCode(err) != "PC023" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("not_after shrink passes", func(t *testing.T) {
		h := newVerifyOnly(t)
		got, err := f.transition(tenant, h.ID, ActionShorten, ptr(time.Now().Add(time.Hour)), ShortenReasonOverlapShortened)
		if err != nil || got.NotAfter == nil || got.NotAfter.After(time.Now().Add(2*time.Hour)) {
			t.Fatalf("shrink must pass: %+v %v", got, err)
		}
	})
	t.Run("revoke without a reason refused", func(t *testing.T) {
		h := newActive(t)
		if err := f.handleUpdate(rt, tenant, h.ID, `status = 'revoked', revoked_by = $2`, f.requester); pgCode(err) != "PC025" {
			t.Fatalf("got %v", err)
		}
		if _, err := f.transition(tenant, h.ID, ActionRevoke, nil, ""); KindOf(err) != KindInvalid {
			t.Fatalf("service: missing reason must be invalid_request, got %v", err)
		}
	})
	t.Run("revoke with a reason outside the enum refused", func(t *testing.T) {
		h := newActive(t)
		if err := f.handleUpdate(rt, tenant, h.ID, `status = 'revoked', revoked_by = $2, revoke_reason = 'because'`, f.requester); pgCode(err) != "PC025" {
			t.Fatalf("got %v", err)
		}
		if _, err := f.transition(tenant, h.ID, ActionRevoke, nil, "because"); KindOf(err) != KindInvalid {
			t.Fatalf("service: unknown reason must be invalid_request, got %v", err)
		}
	})
	t.Run("revoke without revoked_by refused", func(t *testing.T) {
		h := newActive(t)
		if err := f.handleUpdate(rt, tenant, h.ID, `status = 'revoked', revoke_reason = 'tenant_request'`); pgCode(err) != "PC025" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("client status_changed_at overridden", func(t *testing.T) {
		h := newActive(t)
		// No status change: forced back to the old value.
		if err := f.handleUpdate(rt, tenant, h.ID, `status_changed_at = '2000-01-01', not_after = now() + interval '1 day'`); err != nil {
			t.Fatal(err)
		}
		var got time.Time
		readBack := func() time.Time {
			if err := rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT status_changed_at FROM provider_credential_handles WHERE id = $1`, h.ID).Scan(&got)
			}); err != nil {
				t.Fatal(err)
			}
			return got
		}
		if !readBack().Equal(h.StatusChangedAt) {
			t.Fatalf("status_changed_at must stay %s, got %s", h.StatusChangedAt, got)
		}
		// Status change: forced to now().
		if err := f.handleUpdate(rt, tenant, h.ID, `status = 'revoked', revoked_by = $2, revoke_reason = 'tenant_request', status_changed_at = '2000-01-01', revoked_at = '2000-01-01'`, f.requester); err != nil {
			t.Fatal(err)
		}
		if readBack().Year() == 2000 {
			t.Fatal("a client status_changed_at must be overridden on a status change")
		}
	})
}

// TestPCHandleTransition_ImmutableColumns: every column outside the
// transition set is refused by the TRIGGER (as the owner, which has UPDATE
// on every column) and by PRIVILEGE (as the runtime role).
func TestPCHandleTransition_ImmutableColumns(t *testing.T) {
	f := newFx(t)
	owner := ownerPool(t)
	tenant := f.tenant()
	h, _ := f.register(f.spec(tenant, "acme", "k1"))
	cols := map[string]string{
		"id":                    fmt.Sprintf("'%s'", uuid.New()),
		"tenant_id":             fmt.Sprintf("'%s'", f.tenant()),
		"domain":                "'payments'",
		"provider_id":           "'acme2'",
		"purpose":               "'outbound_api'",
		"key_id":                "'k2'",
		"secret_ref":            fmt.Sprintf("'%s'", memRef(tenant, "casino", "acme", "other")),
		"fingerprint":           fmt.Sprintf("'%s'", randomFingerprint(t)),
		"vendor_account_id":     "'m'",
		"not_before":            "not_before - interval '1 second'",
		"activation_request_id": fmt.Sprintf("'%s'", uuid.New()),
		"created_at":            "now() - interval '1 day'",
		"created_by":            fmt.Sprintf("'%s'", uuid.New()),
	}
	for col, val := range cols {
		t.Run(col+"/owner-trigger", func(t *testing.T) {
			if err := f.handleUpdate(owner, tenant, h.ID, col+" = "+val); pgCode(err) != "PC021" {
				t.Fatalf("got %v", err)
			}
		})
		t.Run(col+"/runtime-privilege", func(t *testing.T) {
			if err := f.handleUpdate(f.rt, tenant, h.ID, col+" = "+val); err == nil {
				t.Fatal("the runtime role must not update " + col)
			}
		})
	}
	t.Run("DELETE/runtime", func(t *testing.T) {
		err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM provider_credential_handles WHERE id = $1`, h.ID)
			return err
		})
		if pgCode(err) != "42501" {
			t.Fatalf("got %v", err)
		}
	})
}

func TestPCHandles_RevokedFingerprintCannotBeReRegistered(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	s := f.spec(tenant, "acme", "k1")
	h, secret := f.register(s)
	f.revoke(tenant, h.ID)

	// Same secret bytes, a different ref and key id: refused at filing.
	s2 := f.spec(tenant, "acme", "k2")
	ref := memRef(tenant, "casino", "acme", "again")
	f.mem.Put(ref, secret)
	if _, err := f.sub.FileRequest(context.Background(), f.rt, f.params(s2, ref, secret), ac(f.requester)); KindOf(err) != KindRegistrationRejected || ClassOf(err) != "duplicate_fingerprint" {
		t.Fatalf("got %v (%s)", err, ClassOf(err))
	}
	// And by the global unique key itself, for a request that bypassed the
	// service check (owner-free: the constraint is what binds).
	id, hash, err := f.rawRequestInsert(f.requester, s2, ref, h.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	f.approve(Request{ID: id, TargetTenantID: tenant, ContentHash: hash}, f.approver)
	if _, err := f.apply(Request{ID: id, TargetTenantID: tenant}); KindOf(err) != KindActivationRejected || ClassOf(err) != "unique_violation" {
		t.Fatalf("got %v (%s)", err, ClassOf(err))
	}
}

func TestPCHandles_SameFingerprintSecondTenantRefused(t *testing.T) {
	f := newFx(t)
	a, b := f.tenant(), f.tenant()
	_, secret := f.register(f.spec(a, "acme", "k1"))
	s := f.spec(b, "acme", "k1")
	ref := memRef(b, "casino", "acme", "shared")
	f.mem.Put(ref, secret)
	if _, err := f.sub.FileRequest(context.Background(), f.rt, f.params(s, ref, secret), ac(f.requester)); KindOf(err) != KindRegistrationRejected || ClassOf(err) != "duplicate_fingerprint" {
		t.Fatalf("got %v (%s)", err, ClassOf(err))
	}
}

// TestPCRef_NamespaceCheck: the IMMUTABLE namespace function and the
// pinned-version CHECKs, on both tables.
func TestPCRef_NamespaceCheck(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	other := f.tenant()
	good := memRef(tenant, "casino", "acme", "name")
	bad := map[string]string{
		"foreign tenant":           memRef(other, "casino", "acme", "name"),
		"foreign domain":           memRef(tenant, "payments", "acme", "name"),
		"foreign provider":         memRef(tenant, "casino", "evil", "name"),
		"two namespace markers":    fmt.Sprintf("memory://x/provider-creds/a/provider-creds/%s/casino/acme/name", tenant),
		"overlapping markers":      fmt.Sprintf("memory://x/provider-creds/provider-creds/%s/casino/acme/name", tenant),
		"dot name":                 fmt.Sprintf("memory://x/provider-creds/%s/casino/acme/.", tenant),
		"dotdot name":              fmt.Sprintf("memory://x/provider-creds/%s/casino/acme/..", tenant),
		"extra segment":            fmt.Sprintf("memory://x/provider-creds/%s/casino/acme/name/more", tenant),
		"namespace in query":       fmt.Sprintf("memory://x/elsewhere?p=/provider-creds/%s/casino/acme/name", tenant),
		"namespace in fragment":    fmt.Sprintf("memory://x/elsewhere#/provider-creds/%s/casino/acme/name", tenant),
		"foreign ns, own in query": fmt.Sprintf("memory://x/provider-creds/%s/casino/acme/name?x=/provider-creds/%s/casino/acme/name", other, tenant),
		"awssm without versionId":  fmt.Sprintf("awssm://prefix/provider-creds/%s/casino/acme/name", tenant),
		"awssm with AWSCURRENT":    fmt.Sprintf("awssm://prefix/provider-creds/%s/casino/acme/name?versionId=AWSCURRENT", tenant),
		"awssm with stage label":   fmt.Sprintf("awssm://prefix/provider-creds/%s/casino/acme/name?versionStage=AWSCURRENT", tenant),
		"devfile without version":  fmt.Sprintf("devfile://provider-creds/%s/casino/acme/name", tenant),
		"unknown scheme":           fmt.Sprintf("file://provider-creds/%s/casino/acme/name", tenant),
		"whitespace":               fmt.Sprintf("memory://x/provider-creds/%s/casino/acme/na me", tenant),
	}
	s := f.spec(tenant, "acme", "k1")
	t.Run("requests/good", func(t *testing.T) {
		if _, _, err := f.rawRequestInsert(f.requester, s, good, randomFingerprint(t)); err != nil {
			t.Fatalf("a well-formed ref must be accepted: %v", err)
		}
	})
	for name, ref := range bad {
		t.Run("requests/"+name, func(t *testing.T) {
			if _, _, err := f.rawRequestInsert(f.requester, s, ref, randomFingerprint(t)); pgCode(err) != "23514" {
				t.Fatalf("got %v", err)
			}
		})
	}
	// The handle table carries the same CHECKs (evaluated before the
	// consume trigger).
	r, _ := f.file(s)
	f.approve(r, f.approver)
	for name, ref := range bad {
		t.Run("handles/"+name, func(t *testing.T) {
			if err := f.directHandleInsert(tenant, r, func(h *Handle) { h.SecretRef = ref }); pgCode(err) != "23514" {
				t.Fatalf("got %v", err)
			}
		})
	}
	// A pinned awssm ref with a JSON key is accepted by the CHECK.
	t.Run("requests/awssm pinned", func(t *testing.T) {
		ref := fmt.Sprintf("awssm://prefix/provider-creds/%s/casino/acme/name?versionId=%s#apiKey", tenant, uuid.NewString())
		if _, _, err := f.rawRequestInsert(f.requester, f.spec(tenant, "acme", "k7"), ref, randomFingerprint(t)); err != nil {
			t.Fatalf("a pinned awssm ref must be accepted: %v", err)
		}
	})
}
