//go:build integration

// Package prooftest (integration builds ONLY: it is behind the integration build
// tag so no production binary can contain it) provisions a per-process, randomly generated actor-proof
// signing key for integration tests (SIGNED-ACTOR-PROOF, ADR 0110). It connects
// as the OWNER/migration role (TEST_DATABASE_URL) - the only role that may write
// the owner-only actor_proof_keys table - inserts the key, and installs the
// matching issuer as the process default. Production code never imports this
// package; no key is ever committed.
package prooftest

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/capability"
)

var (
	mu     sync.Mutex
	loaded = map[string]bool{}
)

// NewKey returns 32 random bytes.
func NewKey(t testing.TB) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("prooftest: random key: %v", err)
	}
	return b
}

// Provision inserts key under kid as an ACTIVE key through the owner URL.
func Provision(t testing.TB, ownerURL, kid string, key []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatalf("prooftest: connect owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `INSERT INTO actor_proof_keys (kid, secret) VALUES ($1, $2)`, kid, key); err != nil {
		t.Fatalf("prooftest: provision key %q: %v", kid, err)
	}
}

// Retire marks kid retired through the owner URL.
func Retire(t testing.TB, ownerURL, kid string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatalf("prooftest: connect owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `UPDATE actor_proof_keys SET status = 'retired', retired_at = now() WHERE kid = $1`, kid); err != nil {
		t.Fatalf("prooftest: retire key %q: %v", kid, err)
	}
}

// RandomKID returns a fresh unique kid.
func RandomKID(t testing.TB) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("prooftest: random kid: %v", err)
	}
	return "t" + hex.EncodeToString(b)
}

// process is the ONE per-process issuer: a random kid/key generated on first
// use. The same key is provisioned (idempotently) into every database a test
// process touches, shared or scratch, via Install.
var process *actorproof.Issuer
var processKID string
var processKey []byte

// Install makes sure this process has a random signing key, provisions it as an
// ACTIVE key into the database at ownerURL (owner role; idempotent) and
// installs the issuer as the process default. Safe to call from many tests and
// for many databases. For a scratch database call it AFTER migration 0120 has
// been applied.
func Install(t testing.TB, ownerURL string) *actorproof.Issuer {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	ensureProcessLocked(t)
	if !loaded[ownerURL] {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, ownerURL)
		if err != nil {
			t.Fatalf("prooftest: connect owner: %v", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, `INSERT INTO actor_proof_keys (kid, secret) VALUES ($1, $2) ON CONFLICT (kid) DO NOTHING`, processKID, processKey); err != nil {
			t.Fatalf("prooftest: provision process key: %v", err)
		}
		loaded[ownerURL] = true
	}
	actorproof.SetDefault(process)
	return process
}

func ensureProcessLocked(t testing.TB) {
	t.Helper()
	if process != nil {
		return
	}
	processKID = RandomKID(t)
	processKey = NewKey(t)
	iss, err := actorproof.NewIssuer(processKID, map[string][]byte{processKID: processKey})
	if err != nil {
		t.Fatalf("prooftest: issuer: %v", err)
	}
	process = iss
	// K1 fixtures open their own sessions and call internal/capability without an
	// authenticated HTTP principal: sign for the session's own actor.
	capability.TestSessionProofHook = AttachForSession
}

// Issuer installs the per-process issuer as the default WITHOUT touching any
// database (for worlds on a scratch database migrated below 0120, which has no
// key table and no proof requirement).
func Issuer(t testing.TB) *actorproof.Issuer {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	ensureProcessLocked(t)
	actorproof.SetDefault(process)
	return process
}

// Execer is satisfied by *pgxpool.Pool (the pool's raw pgx pool) and *pgx.Conn.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// InstallVia is Install for a database reachable through an OWNER-role pool the
// test already holds (typically a scratch database: pass the pool's raw pgx pool). key is the
// per-database cache key (any unique string, e.g. t.Name()).
func InstallVia(t testing.TB, ex Execer, cacheKey string) *actorproof.Issuer {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	ensureProcessLocked(t)
	if !loaded[cacheKey] {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := ex.Exec(ctx, `INSERT INTO actor_proof_keys (kid, secret) VALUES ($1, $2) ON CONFLICT (kid) DO NOTHING`, processKID, processKey); err != nil {
			t.Fatalf("prooftest: provision process key: %v", err)
		}
		loaded[cacheKey] = true
	}
	actorproof.SetDefault(process)
	return process
}

// AttachForSession signs, with the process issuer, a proof for the actor the
// transaction's CURRENT session GUCs resolve to (financial_actor_session()) and
// attaches it to tx. It simulates the server-side issuer for tests that insert
// into a governed table with raw SQL inside a session they opened themselves.
// It signs for whoever the session names, exactly as the real issuer signs for
// the authenticated token subject.
func AttachForSession(ctx context.Context, tx pgx.Tx, operation, target, payloadHash string) error {
	iss := actorproof.Default()
	if iss == nil {
		return actorproof.ErrNoIssuer
	}
	var actor, scope string
	var tenantID *string
	if err := tx.QueryRow(ctx, `SELECT actor::text, tenant::text, scope FROM financial_actor_session()`).Scan(&actor, &tenantID, &scope); err != nil {
		return err
	}
	a, err := uuid.Parse(actor)
	if err != nil {
		return err
	}
	tn := uuid.Nil
	if tenantID != nil {
		if tn, err = uuid.Parse(*tenantID); err != nil {
			return err
		}
	}
	err = iss.Attach(ctx, tx, actorproof.Claims{Actor: a, Scope: scope, Tenant: tn, Operation: operation, Target: target, PayloadHash: payloadHash})
	if errors.Is(err, actorproof.ErrInvalidClaims) {
		return nil // an unsignable session (e.g. platform scope) attaches nothing; the database refuses
	}
	return err
}

// ProcessKey returns the process kid and a copy of its key (for tests that need
// a second issuer, a wrong-key issuer or a forged token).
func ProcessKey(t testing.TB, ownerURL string) (string, []byte) {
	t.Helper()
	Install(t, ownerURL)
	mu.Lock()
	defer mu.Unlock()
	return processKID, append([]byte(nil), processKey...)
}

// Forger builds proof tokens independently of the production signer (a second
// implementation of the v1 wire format), so tests can express a wrong key, a
// bad field, an expired or future window, exactly.
type Forger struct {
	KID string
	Key []byte
}

// Nonce22 returns a fresh 22-character URL-safe nonce.
func Nonce22(t testing.TB) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Token signs exactly the given fields and window.
func (f Forger) Token(t testing.TB, actor uuid.UUID, scope string, tenantID uuid.UUID, op, target, payload string, iat, exp int64) string {
	t.Helper()
	tenantField := ""
	if tenantID != uuid.Nil {
		tenantField = tenantID.String()
	}
	signed := strings.Join([]string{"v1", f.KID, actor.String(), scope, tenantField, op, target, payload,
		strconv.FormatInt(iat, 10), strconv.FormatInt(exp, 10), Nonce22(t)}, "|")
	m := hmac.New(sha256.New, f.Key)
	m.Write([]byte(signed))
	return signed + "|" + hex.EncodeToString(m.Sum(nil))
}

// Valid is a fresh, in-window (30 s) token.
func (f Forger) Valid(t testing.TB, actor uuid.UUID, scope string, tenantID uuid.UUID, op, target, payload string) string {
	t.Helper()
	now := time.Now().Unix()
	return f.Token(t, actor, scope, tenantID, op, target, payload, now, now+30)
}

// ProcessForger is a Forger holding the per-process key (provisioned into the
// database at ownerURL), i.e. a correctly-keyed independent signer.
func ProcessForger(t testing.TB, ownerURL string) Forger {
	t.Helper()
	kid, key := ProcessKey(t, ownerURL)
	return Forger{KID: kid, Key: key}
}

// AttachK1Request attaches the session actor's proof for a raw INSERT into
// staff_capability_grant_requests whose valid_from the test supplies explicitly.
func AttachK1Request(ctx context.Context, tx pgx.Tx, tenantID, grantee uuid.UUID, capabilityName string, validFrom time.Time, validUntil *time.Time, reason string) error {
	vf := validFrom
	return AttachForSession(ctx, tx, actorproof.OpGrantRequest, actorproof.TargetNew, actorproof.Digest(
		actorproof.S(tenantID.String()), actorproof.S(grantee.String()), actorproof.S(capabilityName),
		actorproof.TS(&vf), actorproof.TS(validUntil), actorproof.S(reason)))
}

// k1RequestDigest recomputes a stored request's content digest as the trigger does.
func k1RequestDigest(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) (string, error) {
	var (
		id, tenantID, grantee  uuid.UUID
		capabilityName, reason string
		validFrom              time.Time
		validUntil             *time.Time
	)
	if err := tx.QueryRow(ctx, `SELECT id, tenant_id, grantee_staff_id, capability, valid_from, valid_until, reason_code
		FROM staff_capability_grant_requests WHERE id = $1`, requestID).
		Scan(&id, &tenantID, &grantee, &capabilityName, &validFrom, &validUntil, &reason); err != nil {
		return "", err
	}
	return actorproof.Digest(actorproof.S(id.String()), actorproof.S(tenantID.String()), actorproof.S(grantee.String()),
		actorproof.S(capabilityName), actorproof.TS(&validFrom), actorproof.TS(validUntil), actorproof.S(reason)), nil
}

// AttachK1Decision attaches the session actor's proof for a raw INSERT into
// staff_capability_grant_approvals (decision is "approve" or "reject").
func AttachK1Decision(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, decision string) error {
	d, err := k1RequestDigest(ctx, tx, requestID)
	if err != nil {
		return err
	}
	return AttachForSession(ctx, tx, "capability_grant:"+decision, requestID.String(), d)
}

// AttachK1Revoke attaches the session actor's proof for a raw revoke UPDATE.
func AttachK1Revoke(ctx context.Context, tx pgx.Tx, grantID, tenantID uuid.UUID, reason string) error {
	return AttachForSession(ctx, tx, actorproof.OpGrantRevoke, grantID.String(), actorproof.Digest(
		actorproof.S(grantID.String()), actorproof.S(tenantID.String()), actorproof.S(reason)))
}

// K1RequestDigest returns the content digest of a stored K1 request (the
// approval/cancel binding), computed as the trigger computes it.
func K1RequestDigest(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) (string, error) {
	return k1RequestDigest(ctx, tx, requestID)
}
