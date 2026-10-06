// Package actorproof is the server-side ISSUER of signed actor proofs
// (PRH-2 R5, SIGNED-ACTOR-PROOF, ADR 0110, migration 0120).
//
// Threat: arbitrary SQL with a stolen igaming_runtime credential can set the
// transaction-local GUCs that financial_actor_session() derives the four-eyes
// actor from, and so approve as any real admin. Migration 0120 therefore
// requires, on every governed INSERT (and the initiator's cancel), a proof
// signed by THIS process after the HTTP layer authenticated the principal and
// authorised the action. The database verifies the HMAC with a key the runtime
// role cannot read (actor_proof_keys, owner-only) and consumes the nonce
// (actor_proof_nonces, owner-only, UNIQUE).
//
// Wire format (v1), ASCII, '|' separated, 12 fields:
//
//	v1|kid|actor|scope|tenant|operation|target|payload_hash|iat|exp|nonce|mac
//
// mac = lower-hex HMAC-SHA256(secret(kid), fields 1..11 joined by '|').
//
// The signing capability (Issuer.Sign / Issuer.Attach) must be reached ONLY
// from the packages that own four-eyes and the K1 capability-grant flow
// (internal/adjustment, internal/payments, internal/capability) and from process
// wiring; a static test (TestStatic_OnlyFourEyesPackagesSign in
// actorproof_test.go) pins this. The key is
// configuration (ACTOR_PROOF_KEYS / ACTOR_PROOF_ACTIVE_KID), never committed,
// never stored where igaming_runtime SQL can read it.
package actorproof

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// GUC is the transaction-local setting the proof travels in.
const GUC = "app.actor_proof"

// Lifetime is the validity window the issuer uses; the database refuses any
// proof whose exp-iat exceeds 60 seconds.
const Lifetime = 30 * time.Second

// MinKeyBytes is the shortest accepted signing key (matches the database
// CHECK on actor_proof_keys.secret).
const MinKeyBytes = 32

// Operations the database binds (migration 0120).
const (
	OpAdjustmentInitiate = "ledger_adjustment:initiate"
	OpAdjustmentApprove  = "ledger_adjustment:approve"
	OpAdjustmentReject   = "ledger_adjustment:reject"
	OpAdjustmentCancel   = "ledger_adjustment:cancel"
	OpResolutionRequest  = "payment_force_resolve:request"
	OpResolutionApprove  = "payment_force_resolve:approve"
	OpResolutionReject   = "payment_force_resolve:reject"
	OpResolutionCancel   = "payment_force_resolve:cancel"

	// K1 capability grants and financial policy changes (scope tenant or platform).
	OpGrantRequest  = "capability_grant:request"
	OpGrantApprove  = "capability_grant:approve"
	OpGrantReject   = "capability_grant:reject"
	OpGrantCancel   = "capability_grant:cancel"
	OpGrantRevoke   = "capability_grant:revoke"
	OpPolicyPropose = "financial_policy_change:propose"
	OpPolicyApprove = "financial_policy_change:approve"
	OpPolicyReject  = "financial_policy_change:reject"
	OpPolicyCancel  = "financial_policy_change:cancel"
)

// Scopes the database admits.
const (
	ScopeTenant         = "tenant"
	ScopePlatformActing = "platform_acting"
	// ScopePlatform is the plain platform session. It is provable ONLY for the
	// capability_grant: and financial_policy_change: operations and carries a
	// NULL tenant (an empty tenant field on the wire).
	ScopePlatform = "platform"
)

// TargetNew is the target of a create whose id the database forces.
const TargetNew = "new"

var (
	// ErrNoIssuer: no issuer is configured. Governed writes fail closed.
	ErrNoIssuer = errors.New("actorproof: no signing key configured (governed writes fail closed)")
	// ErrInvalidClaims: the claims are not signable.
	ErrInvalidClaims = errors.New("actorproof: invalid claims")
	// ErrKeyConfig: the key configuration is unusable.
	ErrKeyConfig = errors.New("actorproof: invalid key configuration")
)

var (
	kidRE     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	hex64RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	opRE      = regexp.MustCompile(`^[a-z_]+:[a-z_]+$`)
	targetRE  = regexp.MustCompile(`^(new|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
	scopeSet  = map[string]bool{ScopeTenant: true, ScopePlatformActing: true, ScopePlatform: true}
	keyPairRE = regexp.MustCompile(`^([A-Za-z0-9._-]{1,32}):([A-Za-z0-9+/_=-]+)$`)
)

// Claims is exactly what a proof binds.
type Claims struct {
	Actor       uuid.UUID
	Scope       string
	Tenant      uuid.UUID
	Operation   string
	Target      string // a uuid, or TargetNew
	PayloadHash string // 64 lowercase hex
}

func (c Claims) validate() error {
	switch {
	case c.Actor == uuid.Nil:
		return fmt.Errorf("%w: nil actor", ErrInvalidClaims)
	case !scopeSet[c.Scope]:
		return fmt.Errorf("%w: scope", ErrInvalidClaims)
	case c.Scope == ScopePlatform && (c.Tenant != uuid.Nil || !platformOperation(c.Operation)):
		return fmt.Errorf("%w: platform scope needs a nil tenant and a K1/policy operation", ErrInvalidClaims)
	case c.Scope != ScopePlatform && c.Tenant == uuid.Nil:
		return fmt.Errorf("%w: nil tenant", ErrInvalidClaims)
	case !opRE.MatchString(c.Operation):
		return fmt.Errorf("%w: operation", ErrInvalidClaims)
	case !targetRE.MatchString(c.Target):
		return fmt.Errorf("%w: target", ErrInvalidClaims)
	case !hex64RE.MatchString(c.PayloadHash):
		return fmt.Errorf("%w: payload hash", ErrInvalidClaims)
	}
	return nil
}

func platformOperation(op string) bool {
	return strings.HasPrefix(op, "capability_grant:") || strings.HasPrefix(op, "financial_policy_change:")
}

// Issuer signs proofs. It holds key material and must be treated as a secret
// holder: it has no exported fields and no String/Format that could print keys.
type Issuer struct {
	activeKID string
	keys      map[string][]byte
	now       func() time.Time
}

// String never renders key material.
func (i *Issuer) String() string { return "actorproof.Issuer{redacted}" }

// GoString never renders key material.
func (i *Issuer) GoString() string { return i.String() }

// NewIssuer builds an Issuer. activeKID must name an entry of keys; every key
// must be at least MinKeyBytes. Keys are copied.
func NewIssuer(activeKID string, keys map[string][]byte) (*Issuer, error) {
	if !kidRE.MatchString(activeKID) {
		return nil, fmt.Errorf("%w: active kid", ErrKeyConfig)
	}
	cp := make(map[string][]byte, len(keys))
	for kid, k := range keys {
		if !kidRE.MatchString(kid) {
			return nil, fmt.Errorf("%w: kid %q", ErrKeyConfig, kid)
		}
		if len(k) < MinKeyBytes {
			return nil, fmt.Errorf("%w: key %q is shorter than %d bytes", ErrKeyConfig, kid, MinKeyBytes)
		}
		cp[kid] = append([]byte(nil), k...)
	}
	if _, ok := cp[activeKID]; !ok {
		return nil, fmt.Errorf("%w: active kid %q has no key", ErrKeyConfig, activeKID)
	}
	return &Issuer{activeKID: activeKID, keys: cp, now: time.Now}, nil
}

// ParseKeySet parses ACTOR_PROOF_KEYS: comma-separated "kid:base64(secret)"
// entries (standard or URL-safe base64, each secret at least MinKeyBytes
// decoded). It never echoes secret material in errors.
func ParseKeySet(raw string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		m := keyPairRE.FindStringSubmatch(part)
		if m == nil {
			return nil, fmt.Errorf("%w: entry is not kid:base64", ErrKeyConfig)
		}
		b, err := base64.StdEncoding.DecodeString(m[2])
		if err != nil {
			b, err = base64.URLEncoding.DecodeString(m[2])
		}
		if err != nil {
			return nil, fmt.Errorf("%w: key %q is not valid base64", ErrKeyConfig, m[1])
		}
		if len(b) < MinKeyBytes {
			return nil, fmt.Errorf("%w: key %q decodes to fewer than %d bytes", ErrKeyConfig, m[1], MinKeyBytes)
		}
		if _, dup := out[m[1]]; dup {
			return nil, fmt.Errorf("%w: duplicate kid %q", ErrKeyConfig, m[1])
		}
		out[m[1]] = b
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no keys", ErrKeyConfig)
	}
	return out, nil
}

// NewIssuerFromConfig builds an Issuer from the two configuration values, or
// returns (nil, nil) when neither is set.
func NewIssuerFromConfig(activeKID, rawKeys string) (*Issuer, error) {
	if strings.TrimSpace(rawKeys) == "" && strings.TrimSpace(activeKID) == "" {
		return nil, nil
	}
	keys, err := ParseKeySet(rawKeys)
	if err != nil {
		return nil, err
	}
	return NewIssuer(activeKID, keys)
}

// KID returns the active key id.
func (i *Issuer) KID() string { return i.activeKID }

// Sign returns a proof for c, signed with the active key. The caller must
// already have authenticated c.Actor and authorised the operation server-side.
func (i *Issuer) Sign(c Claims) (string, error) {
	if i == nil {
		return "", ErrNoIssuer
	}
	if err := c.validate(); err != nil {
		return "", err
	}
	var nb [18]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return "", fmt.Errorf("actorproof: nonce: %w", err)
	}
	now := i.now()
	tenantField := ""
	if c.Tenant != uuid.Nil {
		tenantField = c.Tenant.String()
	}
	signed := strings.Join([]string{
		"v1", i.activeKID, c.Actor.String(), c.Scope, tenantField, c.Operation, c.Target, c.PayloadHash,
		strconv.FormatInt(now.Unix(), 10), strconv.FormatInt(now.Add(Lifetime).Unix(), 10),
		base64.RawURLEncoding.EncodeToString(nb[:]),
	}, "|")
	mac := hmac.New(sha256.New, i.keys[i.activeKID])
	mac.Write([]byte(signed))
	return signed + "|" + hex.EncodeToString(mac.Sum(nil)), nil
}

// Attach signs c and sets the transaction-local GUC app.actor_proof on tx. It
// is called AFTER the session (tenant/acting) is open and BEFORE the governed
// write; the proof is consumed by that write's trigger.
func (i *Issuer) Attach(ctx context.Context, tx pgx.Tx, c Claims) error {
	tok, err := i.Sign(c)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, tok); err != nil {
		return fmt.Errorf("actorproof: set proof: %w", err)
	}
	return nil
}

// Digest is the k2_canonical + SHA-256 digest migration 0120 computes over the
// caller-supplied columns of a create: each field as "<octet length>:<value>",
// nil as "~", joined by ",". Identical to SQL k2_sha256_hex(k2_canonical(...)).
func Digest(fields ...*string) string {
	parts := make([]string, len(fields))
	for n, f := range fields {
		if f == nil {
			parts[n] = "~"
			continue
		}
		parts[n] = strconv.Itoa(len(*f)) + ":" + *f
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(sum[:])
}

// TS is the canonical UTC microsecond text of t, identical to the SQL
// actor_proof_ts(); nil stays nil (SQL NULL).
func TS(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
	return &s
}

// S returns &s, for Digest call sites.
func S(s string) *string { return &s }

// SP returns nil for an empty string, else &s (empty means SQL NULL).
func SP(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// UUIDP returns nil for a nil pointer, else the canonical string.
func UUIDP(u *uuid.UUID) *string {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}

// ---- process-wide default ---------------------------------------------------

var defaultIssuer atomic.Pointer[Issuer]

// SetDefault installs the process issuer (startup wiring, and test setup).
func SetDefault(i *Issuer) { defaultIssuer.Store(i) }

// Default returns the process issuer, or nil.
func Default() *Issuer { return defaultIssuer.Load() }

// ---- production startup gate ------------------------------------------------

// KeyChecker is satisfied by *db.Pool (ActorProofKeyActive) and by fakes in
// unit tests.
type KeyChecker interface {
	ActorProofKeyActive(ctx context.Context, kid string) (bool, error)
}

// VerifyConfiguredInProduction is the fail-closed production startup gate
// (VerifyRuntimeRoleInProduction style). Outside production it is a no-op.
// In production it refuses to start unless an Issuer is configured AND the
// database holds that kid as an ACTIVE verification key (else every governed
// write would fail closed at runtime, and the operator should learn at deploy
// time, not at the first four-eyes approval). environment is the value of
// config.GuardEnvironment().
func VerifyConfiguredInProduction(ctx context.Context, environment string, iss *Issuer, c KeyChecker) error {
	if environment != "production" {
		return nil
	}
	if iss == nil {
		return fmt.Errorf("actorproof: production requires ACTOR_PROOF_KEYS and ACTOR_PROOF_ACTIVE_KID (SIGNED-ACTOR-PROOF, ADR 0110); refusing to start")
	}
	ok, err := c.ActorProofKeyActive(ctx, iss.KID())
	if err != nil {
		return fmt.Errorf("actorproof: check the active kid is provisioned in the database: %w", err)
	}
	if !ok {
		return fmt.Errorf("actorproof: the configured active kid %q is not an ACTIVE key in actor_proof_keys; provision it (docs/runbooks/operational-runbooks.md section 15); refusing to start", iss.KID())
	}
	return nil
}
