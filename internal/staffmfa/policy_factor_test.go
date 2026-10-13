package staffmfa

import (
	"errors"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/staffmfa/totp"
)

// t0 is a fixed, injected clock origin (deterministic tests; no time.Now).
var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func TestPolicyConstants_Pinned(t *testing.T) {
	if ChallengeTTL != 5*time.Minute || ChallengeAttempts != 5 || PendingFactorTTL != 10*time.Minute {
		t.Fatal("challenge / pending constants changed (ADR 0119 section 7.4)")
	}
	if SoftLockThreshold != 5 || SoftLockWindow != 15*time.Minute || SoftLockDuration != 15*time.Minute ||
		HardLockThreshold != 10 || HardLockWindow != 24*time.Hour {
		t.Fatal("lockout constants changed (ADR 0119 section 6.5)")
	}
	for kind, want := range map[MACKind]time.Duration{MACEnrolInvite: 72 * time.Hour, MACEnrolMFAReset: 24 * time.Hour,
		MACEnrolPasswordReset: 24 * time.Hour, MACEnrolBootstrap: 24 * time.Hour} {
		got, err := EnrolmentTokenTTL(kind)
		if err != nil || got != want || got > MaxEnrolmentTokenTTL {
			t.Fatalf("token TTL of kind %d = %s, %v", kind, got, err)
		}
	}
	for _, kind := range []MACKind{0, MACRecoveryCode, MACChallengeMFA, MACChallengeEnrol, 99} {
		if _, err := EnrolmentTokenTTL(kind); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("kind %d is not an enrolment token", kind)
		}
	}
}

func TestBounds_Section7_4(t *testing.T) {
	cases := []struct {
		b           Bound
		def, lo, hi time.Duration
		name        string
	}{
		{StaffAccessTokenTTLBound, 15 * time.Minute, time.Minute, 15 * time.Minute, "STAFF_ACCESS_TOKEN_TTL_SECONDS"},
		{StaffRefreshIdleTTLBound, 30 * time.Minute, 5 * time.Minute, 60 * time.Minute, "STAFF_REFRESH_IDLE_TTL_SECONDS"},
		{StaffSessionAbsoluteTTLBound, 12 * time.Hour, time.Hour, 12 * time.Hour, "STAFF_SESSION_ABSOLUTE_TTL_SECONDS"},
		{StepUpCriticalMaxAgeBound, 5 * time.Minute, time.Minute, 5 * time.Minute, "STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS"},
		{StepUpFinancialMaxAgeBound, 10 * time.Minute, time.Minute, 10 * time.Minute, "STAFF_STEP_UP_FINANCIAL_MAX_AGE_SECONDS"},
	}
	for _, tc := range cases {
		if tc.b.Default != tc.def || tc.b.Min != tc.lo || tc.b.Max != tc.hi || tc.b.Name != tc.name {
			t.Fatalf("%s bound changed", tc.name)
		}
		for _, ok := range []time.Duration{tc.lo, tc.def, tc.hi} {
			if err := tc.b.Check(ok); err != nil {
				t.Fatalf("%s: %s must pass: %v", tc.name, ok, err)
			}
		}
		for _, bad := range []time.Duration{tc.lo - time.Second, tc.hi + time.Second, 0, -time.Minute} {
			if err := tc.b.Check(bad); err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("%s: %s must be refused, got %v", tc.name, bad, err)
			}
		}
	}
}

// ---- counters (ADR 0119 section 6.5) -------------------------------------------

func failN(c Counters, at time.Time, n int, step time.Duration) (Counters, time.Time, bool) {
	hard := false
	for i := 0; i < n; i++ {
		var h bool
		c, h = c.RecordFailure(at)
		hard = hard || h
		at = at.Add(step)
	}
	return c, at, hard
}

func TestCounters_ZeroIsOpen_ZeroLockStateIsInvalid(t *testing.T) {
	if (Counters{}).LockState(t0) != LockOpen {
		t.Fatal("a fresh factor must be open")
	}
	var s LockState
	if s != LockInvalid || !s.Locked() || LockSoft.Locked() != true || LockHard.Locked() != true || LockOpen.Locked() {
		t.Fatal("lock-state semantics changed")
	}
}

func TestCounters_SoftLock_FiveWithinFifteenMinutes(t *testing.T) {
	c, at, _ := failN(Counters{}, t0, 4, time.Minute)
	if c.LockState(at) != LockOpen || c.FailedCount != 4 {
		t.Fatalf("4 failures must not lock: %+v", c)
	}
	c, _ = c.RecordFailure(at) // the 5th, at t0+4m
	if c.LockState(at) != LockSoft {
		t.Fatal("the 5th failure must soft-lock")
	}
	if !c.LockedUntil.Equal(at.Add(15 * time.Minute)) {
		t.Fatalf("locked_until = %v", c.LockedUntil)
	}
	if c.LockState(c.LockedUntil.Add(-time.Nanosecond)) != LockSoft {
		t.Fatal("still locked just before locked_until")
	}
	if c.LockState(c.LockedUntil) != LockOpen {
		t.Fatal("open at locked_until")
	}
	// After the lock the counter window has reset: 4 more failures do not lock.
	c2, at2, _ := failN(c, c.LockedUntil, 4, time.Second)
	if c2.LockState(at2) != LockOpen || c2.FailedCount != 4 {
		t.Fatalf("the window must restart after the lock: %+v", c2)
	}
}

func TestCounters_SoftWindowExpires(t *testing.T) {
	c, _, _ := failN(Counters{}, t0, 4, 0)
	// Exactly 15 minutes after the window start, the window has closed.
	c1, _ := c.RecordFailure(t0.Add(SoftLockWindow))
	if c1.FailedCount != 1 || !c1.FailedWindowStart.Equal(t0.Add(SoftLockWindow)) || c1.LockState(t0.Add(SoftLockWindow)) != LockOpen {
		t.Fatalf("expired window must restart at 1: %+v", c1)
	}
	// One second before, it is still open, and the 5th failure locks.
	c2, _ := c.RecordFailure(t0.Add(SoftLockWindow - time.Second))
	if c2.LockState(t0.Add(SoftLockWindow-time.Second)) != LockSoft {
		t.Fatal("a 5th failure inside the window must lock")
	}
}

func TestCounters_ClockBackwardsKeepsWindowOpen(t *testing.T) {
	c, _, _ := failN(Counters{}, t0, 4, 0)
	back := t0.Add(-time.Hour)
	c, _ = c.RecordFailure(back)
	if c.LockState(back) != LockSoft {
		t.Fatal("a clock that went backwards must not reset the window")
	}
}

func TestCounters_HardLock_TenWithin24h(t *testing.T) {
	// 4 failures, wait out nothing (no lock), then spaced so the soft window
	// keeps restarting: the 24 h count still accumulates.
	c := Counters{}
	at := t0
	var hard bool
	for i := 1; i <= 9; i++ {
		c, hard = c.RecordFailure(at)
		if hard || c.LockState(at) == LockHard {
			t.Fatalf("hard lock after %d failures", i)
		}
		at = at.Add(20 * time.Minute) // past every soft window and lock
	}
	if c.Failed24hCount != 9 {
		t.Fatalf("24h count = %d", c.Failed24hCount)
	}
	c, hard = c.RecordFailure(at) // the 10th, at t0 + 3h
	if !hard || c.LockState(at) != LockHard || !c.HardLockedAt.Equal(at) {
		t.Fatalf("the 10th failure within 24h must hard-lock: %+v", c)
	}
	// A hard lock does not expire.
	if c.LockState(at.Add(365*24*time.Hour)) != LockHard {
		t.Fatal("a hard lock must never expire")
	}
	// A later failure (callers do not reach it, but if they do) does not
	// re-report the hard lock.
	if _, again := c.RecordFailure(at); again {
		t.Fatal("hard lock reported twice")
	}
}

func TestCounters_24hWindowExpires(t *testing.T) {
	c, _, _ := failN(Counters{}, t0, 9, 20*time.Minute)
	c, hard := c.RecordFailure(t0.Add(HardLockWindow))
	if hard || c.Failed24hCount != 1 || c.LockState(t0.Add(HardLockWindow)) == LockHard {
		t.Fatalf("an expired 24h window must restart: %+v", c)
	}
	c2, _, _ := failN(Counters{}, t0, 9, 20*time.Minute)
	if _, hard := c2.RecordFailure(t0.Add(HardLockWindow - time.Second)); !hard {
		t.Fatal("the 10th failure one second inside 24h must hard-lock")
	}
}

func TestCounters_SuccessResetsSoftWindowOnly(t *testing.T) {
	c, at, _ := failN(Counters{}, t0, 4, time.Minute)
	c = c.RecordSuccess()
	if c.FailedCount != 0 || !c.FailedWindowStart.IsZero() || c.Failed24hCount != 4 || c.LockState(at) != LockOpen {
		t.Fatalf("success: %+v", c)
	}
	c, _ = c.RecordFailure(at)
	if c.FailedCount != 1 || c.LockState(at) != LockOpen {
		t.Fatal("a failure after success starts a new soft window")
	}
}

func TestCounters_InvalidStatesFailClosed(t *testing.T) {
	bad := []Counters{
		{FailedCount: -1},
		{FailedCount: 1},                        // count without a window start
		{FailedWindowStart: t0},                 // window start without a count
		{FailedCount: 5, FailedWindowStart: t0}, // at the threshold but not locked
		{Failed24hCount: -1},
		{Failed24hCount: 1},
		{Failed24hStart: t0},
		{Failed24hCount: 11, Failed24hStart: t0},
		{Failed24hCount: 10, Failed24hStart: t0}, // at the hard-lock threshold without hard_locked_at (C4)
	}
	for i, c := range bad {
		if s := c.LockState(t0); s != LockInvalid || !s.Locked() {
			t.Fatalf("case %d: %v", i, s)
		}
	}
	// A hard lock wins over invalid counters.
	if (Counters{FailedCount: -1, HardLockedAt: t0}).LockState(t0) != LockHard {
		t.Fatal("hard lock must win")
	}
}

// ---- VerifyTOTP (the pure core of StaffFactor.Verify) ----------------------------

func storedFactor(t *testing.T, k *Keys) (StoredTOTPFactor, FactorSecret) {
	t.Helper()
	aad := testAAD()
	secret := mustSecret(t)
	sealed, err := k.SealFactorSecret(aad, secret)
	if err != nil {
		t.Fatal(err)
	}
	return StoredTOTPFactor{AAD: aad, Sealed: sealed}, secret
}

func codeAt(t *testing.T, s FactorSecret, at time.Time) string {
	t.Helper()
	c, err := totp.CodeAt(s.d().b, at)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVerifyTOTP_AcceptsAndBurns(t *testing.T) {
	k, _ := testKeys(t)
	f, secret := storedFactor(t, k)
	c := codeAt(t, secret, t0)
	r := k.VerifyTOTP(f, NewTOTPAssertion(c), t0)
	step, _ := totp.StepAt(t0)
	if !r.Accepted() || r.AcceptedStep != step || r.AMR != AMROTP {
		t.Fatalf("result %+v", r)
	}
	f.LastUsedStep = r.AcceptedStep
	if r := k.VerifyTOTP(f, NewTOTPAssertion(c), t0); r.Outcome != OutcomeRejected || r.Accepted() {
		t.Fatalf("replay must be rejected: %+v", r)
	}
}

func TestVerifyTOTP_LockedFactorRefusedBeforeAnyComparison(t *testing.T) {
	k, _ := testKeys(t)
	f, secret := storedFactor(t, k)
	correct := NewTOTPAssertion(codeAt(t, secret, t0))
	// Make the sealed secret unopenable: if the lock check came after the
	// open, the outcome would be OutcomeIntegrity, not a lock.
	f.Sealed.Ciphertext = append([]byte(nil), f.Sealed.Ciphertext...)
	f.Sealed.Ciphertext[0] ^= 1

	f.Counters = Counters{LockedUntil: t0.Add(time.Minute)}
	if r := k.VerifyTOTP(f, correct, t0); r.Outcome != OutcomeLocked {
		t.Fatalf("soft-locked: %+v", r)
	}
	f.Counters = Counters{HardLockedAt: t0.Add(-time.Hour), Failed24hCount: 10, Failed24hStart: t0.Add(-2 * time.Hour)}
	if r := k.VerifyTOTP(f, correct, t0); r.Outcome != OutcomeHardLocked {
		t.Fatalf("hard-locked: %+v", r)
	}
	f.Counters = Counters{FailedCount: -3}
	if r := k.VerifyTOTP(f, correct, t0); r.Outcome != OutcomeLocked {
		t.Fatalf("invalid counters: %+v", r)
	}
}

func TestVerifyTOTP_CorrectCodeWhileLockedIsLocked(t *testing.T) {
	k, _ := testKeys(t)
	f, secret := storedFactor(t, k)
	f.Counters = Counters{LockedUntil: t0.Add(time.Second)}
	if r := k.VerifyTOTP(f, NewTOTPAssertion(codeAt(t, secret, t0)), t0); r.Outcome != OutcomeLocked || r.Accepted() {
		t.Fatalf("%+v", r)
	}
	// Once the lock has passed, the same code verifies.
	if r := k.VerifyTOTP(f, NewTOTPAssertion(codeAt(t, secret, t0.Add(time.Second))), t0.Add(time.Second)); !r.Accepted() {
		t.Fatalf("%+v", r)
	}
}

func TestVerifyTOTP_IntegrityAndRejections(t *testing.T) {
	k, _ := testKeys(t)
	f, secret := storedFactor(t, k)
	good := NewTOTPAssertion(codeAt(t, secret, t0))

	moved := f
	moved.AAD.StaffUserID = uuid.New()
	if r := k.VerifyTOTP(moved, good, t0); r.Outcome != OutcomeIntegrity {
		t.Fatalf("moved ciphertext: %+v", r)
	}
	planted := f
	planted.Sealed = attackerSeal(t, f.AAD)
	if r := k.VerifyTOTP(planted, good, t0); r.Outcome != OutcomeIntegrity {
		t.Fatalf("planted factor: %+v", r)
	}
	wrongType := f
	wrongType.AAD.FactorType = "webauthn"
	if r := k.VerifyTOTP(wrongType, good, t0); r.Outcome != OutcomeIntegrity {
		t.Fatalf("non-TOTP factor: %+v", r)
	}
	for name, a := range map[string]Assertion{
		"recovery code kind": NewRecoveryCodeAssertion(codeAt(t, secret, t0)),
		"zero assertion":     {},
		"malformed":          NewTOTPAssertion("12345"),
		"wrong":              NewTOTPAssertion(wrongCode(codeAt(t, secret, t0))),
		"out of window":      NewTOTPAssertion(codeAt(t, secret, t0.Add(2*time.Duration(totp.Period)*time.Second))),
	} {
		if r := k.VerifyTOTP(f, a, t0); r.Outcome != OutcomeRejected || r.Accepted() {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	// Negative last-used step (a CHECK the DB enforces) fails closed.
	neg := f
	neg.LastUsedStep = -1
	if r := k.VerifyTOTP(neg, good, t0); r.Accepted() {
		t.Fatal("negative last_used_step accepted")
	}
	// The zero Result fails closed.
	if (Result{}).Accepted() {
		t.Fatal("the zero Result must not be accepted")
	}
}

func wrongCode(c string) string {
	b := []byte(c)
	b[0] = '0' + (b[0]-'0'+1)%10
	return string(b)
}

func TestFactorType_ClosedVocabulary(t *testing.T) {
	if !FactorTOTP.Valid() || FactorType("webauthn").Valid() || FactorType("").Valid() || FactorType("TOTP").Valid() {
		t.Fatal("factor type vocabulary changed")
	}
	if (StaffRef{}).Valid() || !(StaffRef{StaffID: uuid.New()}).Valid() {
		t.Fatal("StaffRef validity")
	}
	if AMRPassword != "pwd" || AMROTP != "otp" || AMRMFA != "mfa" {
		t.Fatal("RFC 8176 values changed")
	}
}

// ---- otpauth -------------------------------------------------------------------------

func TestOTPAuthURI_Format(t *testing.T) {
	// The RFC 6238 seed is a public test vector.
	s := newFactorSecret([]byte("12345678901234567890"))
	m, err := NewTOTPEnrolmentMaterial(DefaultIssuer, "jane+ops@example.test", s)
	if err != nil {
		t.Fatal(err)
	}
	want := "otpauth://totp/iGaming%20Platform%20Back%20Office:jane%2Bops%40example.test" +
		"?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&issuer=iGaming%20Platform%20Back%20Office&algorithm=SHA1&digits=6&period=30"
	if m.otpauthURI() != want {
		t.Fatalf("uri = %s", m.otpauthURI())
	}
	if m.secretBase32() != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" {
		t.Fatal("base32 secret")
	}
	if DefaultIssuer != "iGaming Platform Back Office" {
		t.Fatal("default issuer changed")
	}
	// Reserved characters in the account are escaped, not interpreted.
	m, err = NewTOTPEnrolmentMaterial("Ops & Co=1", "a/b?c#d&e@x.test", s)
	if err != nil || !strings.HasPrefix(m.otpauthURI(), "otpauth://totp/Ops%20%26%20Co%3D1:a%2Fb%3Fc%23d%26e%40x.test?secret=") ||
		!strings.Contains(m.otpauthURI(), "&issuer=Ops%20%26%20Co%3D1&algorithm=") {
		t.Fatalf("escaping: %v", err)
	}
	// Parsing the URI back yields exactly one issuer and the original values.
	u, err := url.Parse(m.otpauthURI())
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if len(q["issuer"]) != 1 || q.Get("issuer") != "Ops & Co=1" || len(q) != 5 {
		t.Fatalf("round trip: %v", q)
	}
	if strings.Contains(m.otpauthURI(), "+") {
		t.Fatal("a '+' in the URI would read as a space or a literal plus depending on the app")
	}
}

func TestOTPAuthURI_FailsClosed(t *testing.T) {
	s := mustSecret(t)
	bad := []struct{ issuer, account string }{
		{"", "a@x.test"},
		{"Ops", ""},
		{"Ops:Evil", "a@x.test"},
		{"Ops", "a:b@x.test"},
		{" Ops", "a@x.test"},
		{"Ops ", "a@x.test"},
		{"Ops\n", "a@x.test"},
		{"Ops\x7f", "a@x.test"},
		{"Ops\u0085", "a@x.test"},
		{"O\xffps", "a@x.test"},
		{strings.Repeat("o", 65), "a@x.test"},
		{"Ops", strings.Repeat("a", 255)},
	}
	for _, tc := range bad {
		if _, err := NewTOTPEnrolmentMaterial(tc.issuer, tc.account, s); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%q / %q must be refused", tc.issuer, tc.account)
		}
	}
	if _, err := NewTOTPEnrolmentMaterial("Ops", "a@x.test", newFactorSecret(randBytes(t, 16))); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("a short secret must be refused")
	}
	if err := ValidateIssuer(DefaultIssuer); err != nil {
		t.Fatal(err)
	}
	if err := ValidateIssuer("Brand:X"); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("issuer with ':' must be refused")
	}
	if err := ValidateIssuer(strings.Repeat("é", 32)); err != nil {
		t.Fatalf("64-byte UTF-8 issuer: %v", err)
	}
}

// ---- startup gate ----------------------------------------------------------------------

func TestVerifyStartup(t *testing.T) {
	if err := VerifyStartup(nil); !errors.Is(err, ErrStartupGate) || !strings.Contains(err.Error(), EnvFactorKeys) || !strings.Contains(err.Error(), EnvMACKeys) {
		t.Fatalf("nil keys must refuse in every environment: %v", err)
	}
	k, _ := testKeys(t)
	if err := VerifyStartup(k); err != nil {
		t.Fatal(err)
	}
	// A broken factor subkey is caught by the self-check.
	k.m().factor[k.m().factorActive] = []byte("short")
	if err := VerifyStartup(k); !errors.Is(err, ErrStartupGate) {
		t.Fatalf("broken factor key: %v", err)
	}
	// A MAC kid without subkeys (impossible through NewKeys) is caught too.
	k2, _ := testKeys(t)
	k2.m().macActive = "ghost"
	if err := VerifyStartup(k2); !errors.Is(err, ErrStartupGate) {
		t.Fatalf("broken MAC kid: %v", err)
	}
}

// ---- static pins -----------------------------------------------------------------------

// The library never reads the environment, the clock or a mode switch: there
// is no development bypass (ADR 0119 section 4.1), and every time value is
// injected. It imports nothing that does I/O beyond crypto/rand.
func TestStatic_NoBypassNoClockNoIO(t *testing.T) {
	allowed := map[string]bool{
		"context": true, "crypto/aes": true, "crypto/cipher": true, "crypto/hkdf": true, "crypto/hmac": true,
		"crypto/rand": true, "crypto/sha1": true, "crypto/sha256": true, "crypto/subtle": true,
		"encoding/base32": true, "encoding/base64": true, "encoding/binary": true, "encoding/json": true,
		"errors": true, "fmt": true, "io": true, "log/slog": true, "net/url": true, "regexp": true, "sort": true,
		"strconv": true, "strings": true, "time": true, "unicode/utf8": true,
		"github.com/google/uuid": true, "github.com/jackc/pgx/v5": true,
		"github.com/Diansalas/igaming-platform/internal/staffmfa/totp": true,
	}
	forbidden := []string{"os.Getenv", "os.LookupEnv", "time.Now", "APP_ENV", `"development"`, `"production"`, `"staging"`, "Bypass", "SkipMFA", "Insecure"}
	files, _ := filepath.Glob("*.go")
	more, _ := filepath.Glob("totp/*.go")
	files = append(files, more...)
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !allowed[p] {
				t.Fatalf("%s imports %s", path, p)
			}
		}
		for _, bad := range forbidden {
			if strings.Contains(string(src), bad) {
				t.Fatalf("%s contains %q", path, bad)
			}
		}
		checked++
	}
	if checked < 8 {
		t.Fatalf("static scan is vacuous: %d files", checked)
	}
}
