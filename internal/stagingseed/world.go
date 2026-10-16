package stagingseed

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// SyntheticLicenceNumber marks the tenants this tool created: only tenants carrying this licence get staff accounts (SRP3R-4).
const SyntheticLicenceNumber = "STG-SYNTHETIC-1"

// StaffSpec is one synthetic tenant staff account the acceptance suite (deploy/aws/acceptance) consumes. Key is the suite's
// actor name (<KEY>_EMAIL, <KEY>_PASSWORD_FILE, <KEY>_BOOTSTRAP_TOKEN_FILE).
type StaffSpec struct {
	Key   string
	Local string // local part of the e-mail
	Role  identity.StaffRole
}

// WorldStaff is the fixed set of tenant staff the suite needs: finance x2 (two DISTINCT Persons, four-eyes), compliance (KYC
// review), tenant_admin (capability configuration), risk_manager (sportsbook settlement simulation).
var WorldStaff = []StaffSpec{
	{"TENANT_ADMIN", "tenant-admin", identity.StaffRoleTenantAdmin},
	{"FINANCE1", "finance1", identity.StaffRoleFinance},
	{"FINANCE2", "finance2", identity.StaffRoleFinance},
	{"COMPLIANCE", "compliance", identity.StaffRoleCompliance},
	{"RISK_MANAGER", "risk-manager", identity.StaffRoleRiskManager},
}

// WorldParams is the validated input of PlanWorld / ApplyWorld.
type WorldParams struct {
	TenantSlug, BrandSlug, SecondTenantSlug, SecondBrandSlug string
	Country, AuthRef, Operator, Reason                       string
	ActorID                                                  uuid.UUID // uuid.Nil = the sole active platform admin
}

// Validate checks everything that needs no database.
func (p WorldParams) Validate() error {
	for name, v := range map[string]string{"tenant slug": p.TenantSlug, "brand slug": p.BrandSlug, "authorization reference": p.AuthRef, "operator": p.Operator, "reason": p.Reason} {
		if err := CheckField(name, v); err != nil {
			return err
		}
	}
	if (p.SecondTenantSlug == "") != (p.SecondBrandSlug == "") {
		return errors.New("second tenant slug and second brand slug go together")
	}
	if p.SecondTenantSlug != "" {
		if err := CheckField("second tenant slug", p.SecondTenantSlug); err != nil {
			return err
		}
		if err := CheckField("second brand slug", p.SecondBrandSlug); err != nil {
			return err
		}
		if p.SecondTenantSlug == p.TenantSlug {
			return errors.New("the second tenant must differ from the first")
		}
	}
	if !validation.IsISO3166Alpha2(p.Country) {
		return errors.New("country must be a valid, uppercase ISO-3166-1 alpha-2 code")
	}
	return nil
}

// ActorCred is one account's credential material. It lives only in a 0600 file on the task's tmpfs and, from there, in the
// Secrets Manager secret igaming-staging/seed-world-passwords: it is NEVER printed or logged.
type ActorCred struct {
	Email          string `json:"email"`
	Password       string `json:"password,omitempty"`
	BootstrapToken string `json:"bootstrap_token,omitempty"`
}

// Creds is the credentials file format (plan and apply phases).
type Creds struct {
	TenantSlug string                `json:"tenant_slug"`
	BrandSlug  string                `json:"brand_slug"`
	Country    string                `json:"country"`
	Actors     map[string]*ActorCred `json:"actors"`
}

// ReadCreds decodes a credentials file's content.
func ReadCreds(b []byte) (Creds, error) {
	var c Creds
	if err := json.Unmarshal(b, &c); err != nil {
		return Creds{}, fmt.Errorf("credentials file: %w", err)
	}
	return c, nil
}

func (p WorldParams) email(s StaffSpec) string {
	return s.Local + "@" + p.TenantSlug + ".acceptance.invalid"
}

// GeneratePassword returns a 28-character random password (fixed prefix satisfies the complexity rules; 22 base62 characters
// from crypto/rand carry ~130 bits).
func GeneratePassword() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 22)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b[i] = alphabet[n.Int64()]
	}
	return "Sw1-" + string(b), nil
}

// gate applies the two environment gates and resolves the actor (an ACTIVE platform_admin).
func (p WorldParams) gate(ctx context.Context, pool *db.Pool, appEnv string) (uuid.UUID, string, error) {
	if err := CheckAppEnv(appEnv); err != nil {
		return uuid.Nil, "", err
	}
	if err := p.Validate(); err != nil {
		return uuid.Nil, "", err
	}
	anchor, err := RequireSyntheticAnchor(ctx, pool)
	if err != nil {
		return uuid.Nil, "", err
	}
	actor := p.ActorID
	if actor == uuid.Nil {
		if actor, err = SoleActivePlatformAdmin(ctx, pool); err != nil {
			return uuid.Nil, "", err
		}
	} else if err = pool.WithPlatformAdmin(ctx, actor, func(ctx context.Context, tx pgx.Tx) error {
		// an explicit actor must be an ACTIVE platform_admin already at plan time, so a dry run reports a bad actor (SRP3-3)
		return RequireActivePlatformAdmin(ctx, tx, actor)
	}); err != nil {
		return uuid.Nil, "", err
	}
	return actor, anchor, nil
}

// PlanWorld is READ-ONLY: after the gates it reports which staff accounts do not exist yet and generates a password for each of
// them. The caller stores the result in Secrets Manager BEFORE ApplyWorld creates the accounts, so a failed upload can never
// leave an account whose password was lost.
func PlanWorld(ctx context.Context, pool *db.Pool, p WorldParams, appEnv string, out io.Writer) (Creds, error) {
	say := func(k, v string) { _, _ = fmt.Fprintf(out, "SEEDWORLD|%s|%s\n", k, v) }
	actor, anchor, err := p.gate(ctx, pool, appEnv)
	if err != nil {
		return Creds{}, err
	}
	say("anchor", anchor)
	say("actor_id", actor.String())
	for _, slug := range []string{p.TenantSlug, p.SecondTenantSlug} {
		if slug == "" {
			continue
		}
		exists, synthetic, err := syntheticTenantState(ctx, pool, actor, slug)
		if err != nil {
			return Creds{}, err
		}
		if exists && !synthetic {
			return Creds{}, fmt.Errorf("refused: tenant %q exists but does not carry this tool's synthetic licence marker (%s): it was not created by seed-world", slug, SyntheticLicenceNumber)
		}
	}
	c := Creds{TenantSlug: p.TenantSlug, BrandSlug: p.BrandSlug, Country: p.Country, Actors: map[string]*ActorCred{}}
	for _, s := range WorldStaff {
		exists, err := staffExists(ctx, pool, p.TenantSlug, p.email(s))
		if err != nil {
			return Creds{}, err
		}
		if exists {
			say("staff_"+s.Key, "exists (no password is generated; reissue a bootstrap token with seed-admin -issue-bootstrap if it is lost)")
			continue
		}
		pw, err := GeneratePassword()
		if err != nil {
			return Creds{}, err
		}
		c.Actors[s.Key] = &ActorCred{Email: p.email(s), Password: pw}
		say("staff_"+s.Key, "to be created")
	}
	return c, nil
}

// syntheticTenantState reports whether a tenant with the slug exists and whether it carries the synthetic licence marker of this tool.
func syntheticTenantState(ctx context.Context, pool *db.Pool, actor uuid.UUID, slug string) (exists, synthetic bool, err error) {
	err = pool.WithPlatformAdmin(ctx, actor, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT true, COALESCE(l.licence_number = $2, false) FROM tenants t LEFT JOIN licences l ON l.id = t.licence_id WHERE t.slug = $1`, slug, SyntheticLicenceNumber).Scan(&exists, &synthetic)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return exists, synthetic, err
}

func staffExists(ctx context.Context, pool *db.Pool, tenantSlug, email string) (bool, error) {
	var tenantID uuid.UUID
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM tenants WHERE slug = $1`, tenantSlug).Scan(&tenantID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	exists := false
	err = pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := identity.GetStaffUserByEmail(ctx, tx, email)
		if errors.Is(err, identity.ErrNotFound) {
			return nil
		}
		exists = err == nil
		return err
	})
	return exists, err
}

// ApplyWorld provisions, through the platform's own services and the sanctioned owner-provisioning path, the synthetic world the
// acceptance suite needs: for the first tenant (and the optional second one) a licence, an ACTIVE tenant and brand (table-owner
// INSERT with status 'active': the platform records an owner_provisioned launch transition; ADR 0112), the operating-market rows
// (EnsureMarket), and, for the first tenant only, the staff accounts of in (identity.CreateStaffUser, Persons, a bootstrap MFA
// enrolment token each). It returns the credentials file content including the bootstrap tokens. Idempotent.
func ApplyWorld(ctx context.Context, pool *db.Pool, mfa *store.Store, p WorldParams, appEnv string, in Creds, out io.Writer) (Creds, error) {
	say := func(k, v string) { _, _ = fmt.Fprintf(out, "SEEDWORLD|%s|%s\n", k, v) }
	actor, anchor, err := p.gate(ctx, pool, appEnv)
	if err != nil {
		return Creds{}, err
	}
	say("anchor", anchor)
	for key := range in.Actors {
		known := false
		for _, s := range WorldStaff {
			known = known || s.Key == key
		}
		if !known {
			return Creds{}, fmt.Errorf("credentials file names an unknown actor %q", key)
		}
	}
	if in.TenantSlug != p.TenantSlug || in.BrandSlug != p.BrandSlug {
		return Creds{}, errors.New("the credentials file belongs to a different tenant/brand than the parameters")
	}

	type target struct{ tenant, brand string }
	targets := []target{{p.TenantSlug, p.BrandSlug}}
	if p.SecondTenantSlug != "" {
		targets = append(targets, target{p.SecondTenantSlug, p.SecondBrandSlug})
	}
	for _, t := range targets {
		if err := ensureTenantAndBrand(ctx, pool, actor, t.tenant, t.brand, p, say); err != nil {
			return Creds{}, err
		}
		mp := MarketParams{TenantSlug: t.tenant, BrandSlug: t.brand, Country: p.Country, AuthRef: p.AuthRef, Operator: p.Operator, Reason: p.Reason, ActorID: actor}
		if err := EnsureMarket(ctx, pool, mp, prefixWriter{out, "SEEDWORLD|market_" + t.tenant + "|"}); err != nil {
			return Creds{}, err
		}
	}

	res := Creds{TenantSlug: p.TenantSlug, BrandSlug: p.BrandSlug, Country: p.Country, Actors: map[string]*ActorCred{}}
	var tenantID uuid.UUID
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM tenants WHERE slug = $1`, p.TenantSlug).Scan(&tenantID)
	}); err != nil {
		return Creds{}, err
	}
	for _, s := range WorldStaff {
		cred := in.Actors[s.Key]
		if cred == nil {
			say("staff_"+s.Key, "not created in this run (already exists, or no planned credential)")
			continue
		}
		if cred.Email != p.email(s) || cred.Password == "" {
			return Creds{}, fmt.Errorf("credential for %s does not match the plan", s.Key)
		}
		tok, created, err := createStaff(ctx, pool, mfa, tenantID, actor, s, cred, p)
		if err != nil {
			return res, fmt.Errorf("staff %s: %w", s.Key, err) // res holds the accounts created so far: the caller still delivers them
		}
		if !created {
			say("staff_"+s.Key, "exists already (the planned password was NOT applied; discard it)")
			continue
		}
		res.Actors[s.Key] = &ActorCred{Email: cred.Email, Password: cred.Password, BootstrapToken: tok}
		say("staff_"+s.Key, "created with a bootstrap enrolment token (valid "+staffmfa.BootstrapTokenTTL.String()+")")
	}
	return res, nil
}

type prefixWriter struct {
	w      io.Writer
	prefix string
}

// Write relays a SEEDMARKET|k|v line as SEEDWORLD|market_<tenant>|k|v.
func (pw prefixWriter) Write(b []byte) (int, error) {
	line := string(b)
	const lead = "SEEDMARKET|"
	if len(line) >= len(lead) && line[:len(lead)] == lead {
		line = line[len(lead):]
	}
	_, err := io.WriteString(pw.w, pw.prefix+line)
	return len(b), err
}

func ensureTenantAndBrand(ctx context.Context, pool *db.Pool, actor uuid.UUID, tenantSlug, brandSlug string, p WorldParams, say func(k, v string)) error {
	var tenantID, brandID uuid.UUID
	err := pool.WithPlatformAdmin(ctx, actor, func(ctx context.Context, tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM staff_users WHERE id = $1 AND tenant_id IS NULL AND role = 'platform_admin' AND status = 'active' FOR SHARE`, actor).Scan(&one); err != nil {
			return fmt.Errorf("refused: actor %s is not an active platform_admin", actor)
		}
		// synthetic licence (platform licence), idempotent by number
		var jurID, licID uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO jurisdictions (code, name) VALUES ('STG-SYNTHETIC', 'Synthetic staging jurisdiction')
			ON CONFLICT (code) DO UPDATE SET code = EXCLUDED.code RETURNING id`).Scan(&jurID); err != nil {
			return fmt.Errorf("synthetic jurisdiction: %w", err)
		}
		err := tx.QueryRow(ctx, `SELECT id FROM licences WHERE licence_number = '`+SyntheticLicenceNumber+`' AND jurisdiction_id = $1`, jurID).Scan(&licID)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO licences (jurisdiction_id, licensee, licence_number) VALUES ($1, 'platform', '`+SyntheticLicenceNumber+`') RETURNING id`, jurID).Scan(&licID)
		}
		if err != nil {
			return fmt.Errorf("synthetic licence: %w", err)
		}
		var status string
		var existingLicence *uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id, status, licence_id FROM tenants WHERE slug = $1 FOR UPDATE`, tenantSlug).Scan(&tenantID, &status, &existingLicence)
		switch {
		case err == nil:
			if status != "active" {
				return fmt.Errorf("refused: tenant %q exists with status %s; activation is a governed launch decision, not this tool's", tenantSlug, status)
			}
			if existingLicence == nil || *existingLicence != licID {
				return fmt.Errorf("refused: tenant %q exists but does not carry this tool's synthetic licence marker (%s): it was not created by seed-world, and staff accounts are only added to the synthetic tenants this tool created", tenantSlug, SyntheticLicenceNumber)
			}
			say("tenant_"+tenantSlug, "exists (active)")
		case errors.Is(err, pgx.ErrNoRows):
			// the sanctioned OWNER-PROVISIONED path (ADR 0112, migration 0128): the table owner may insert an ACTIVE row and the database
			// records an owner_provisioned launch transition. A non-owner connection is refused by the database itself.
			if err := tx.QueryRow(ctx, `INSERT INTO tenants (name, slug, licensing_model, status, licence_id) VALUES ($1, $2, 'under_platform_licence', 'active', $3) RETURNING id`,
				"Synthetic "+tenantSlug, tenantSlug, licID).Scan(&tenantID); err != nil {
				return fmt.Errorf("provision tenant %q (the connecting role must be the table owner): %w", tenantSlug, err)
			}
			if err := audit.Record(ctx, tx, audit.Entry{
				ActorType: audit.ActorStaff, ActorID: actor, Action: "staging_seed.tenant_owner_provisioned", TargetType: "tenant", TargetID: tenantID.String(),
				SubjectTenantID: tenantID, Outcome: audit.OutcomeSuccess, UserAgent: "cmd/seed-staging-world operator=" + p.Operator, RequestID: "seed-staging-world",
				Metadata: map[string]any{"slug": tenantSlug, "operator": p.Operator, "reason": p.Reason, "authorization_reference": p.AuthRef, "path": "owner_provisioned", "licence_id": licID.String()},
			}); err != nil {
				return err
			}
			say("tenant_"+tenantSlug, "provisioned (active, owner_provisioned)")
		default:
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT id, status FROM brands WHERE slug = $1 AND tenant_id = $2 FOR UPDATE`, brandSlug, tenantID).Scan(&brandID, &status)
		switch {
		case err == nil:
			if status != "active" {
				return fmt.Errorf("refused: brand %q exists with status %s; activation is a governed launch decision, not this tool's", brandSlug, status)
			}
			say("brand_"+brandSlug, "exists (active)")
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO brands (tenant_id, name, slug, status) VALUES ($1, $2, $3, 'active') RETURNING id`, tenantID, "Synthetic "+brandSlug, brandSlug).Scan(&brandID); err != nil {
			return fmt.Errorf("provision brand %q: %w", brandSlug, err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actor, Action: "staging_seed.brand_owner_provisioned", TargetType: "brand", TargetID: brandID.String(),
			Outcome: audit.OutcomeSuccess, RequestID: "seed-staging-world", UserAgent: "cmd/seed-staging-world operator=" + p.Operator,
			Metadata: map[string]any{"slug": brandSlug, "operator": p.Operator, "reason": p.Reason, "authorization_reference": p.AuthRef, "path": "owner_provisioned", "actor": actor.String()},
		}); err != nil {
			return err
		}
		say("brand_"+brandSlug, "provisioned (active, owner_provisioned)")
		return nil
	})
}

// createStaff creates one tenant staff account (a Person, a staff_users row, a bootstrap enrolment token, audit rows) and returns the
// bootstrap token. created is false when the account already exists (nothing is changed).
func createStaff(ctx context.Context, pool *db.Pool, mfa *store.Store, tenantID, actor uuid.UUID, s StaffSpec, cred *ActorCred, p WorldParams) (string, bool, error) {
	hash, err := auth.HashPassword(cred.Password)
	if err != nil {
		return "", false, err
	}
	var tok string
	created := false
	err = pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := identity.GetStaffUserByEmail(ctx, tx, cred.Email); err == nil {
			return nil
		} else if !errors.Is(err, identity.ErrNotFound) {
			return err
		}
		// the Person is created in the same transaction, after the existence check: no orphan Person on the "exists" path or on a failure (SRP3R-7)
		pr, err := identity.CreatePerson(ctx, tx)
		if err != nil {
			return err
		}
		personID := pr.ID
		st, err := identity.CreateStaffUser(ctx, tx, tenantID, cred.Email, hash, s.Role, &personID)
		if err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return err
		}
		token, err := mfa.IssueEnrolmentToken(ctx, tx, staffmfa.StaffRef{StaffID: st.ID, TenantID: tenantID}, store.EnrolBootstrap, nil, nil, now)
		if err != nil {
			return fmt.Errorf("issue bootstrap enrolment token: %w", err)
		}
		tok = deliverToken(token)
		meta := map[string]any{"tool": "cmd/seed-staging-world", "actor": actor.String(), "role": string(s.Role), "operator": p.Operator, "reason": p.Reason, "authorization_reference": p.AuthRef}
		if err := audit.Record(ctx, tx, audit.Entry{TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actor, Action: "staging_seed.staff_created", TargetType: "staff_user", TargetID: st.ID.String(),
			Outcome: audit.OutcomeSuccess, RequestID: "seed-staging-world", Metadata: meta}); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Entry{TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: actor, Action: "staff.bootstrap_token_issued", TargetType: "staff_user", TargetID: st.ID.String(),
			Outcome: audit.OutcomeSuccess, RequestID: "seed-staging-world", Metadata: map[string]any{"tool": "cmd/seed-staging-world", "mode": "create", "operator": p.Operator, "reason": p.Reason, "ttl_hours": int(staffmfa.BootstrapTokenTTL.Hours())}}); err != nil {
			return err
		}
		created = true
		return nil
	})
	return tok, created, err
}

// deliverToken is the single place the raw bootstrap token leaves its type: into the credentials file that becomes the Secrets
// Manager secret (the protected output channel of this deployment job); never into a log.
func deliverToken(t staffmfa.Token) string { return t.RevealForDelivery() }
