//go:build integration

package httpserver

// ADR 0119 section 17.7 and S3 entry gate (f): the full enrol, verify (right and wrong), recover, re-enrol, refresh
// and logout flows run with a slog handler that records EVERYTHING the server logs (debug level, JSON, the server's
// logger AND slog.Default); afterwards the request, response and domain types that hold the secrets are logged and
// formatted ON PURPOSE - directly, in exported fields and in unexported fields of wrapper structs, by pointer, in
// slices and maps, under every fmt verb, through slog's JSON and text handlers and encoding/json. No secret value
// (the TOTP secret, the otpauth URI, a TOTP code, a recovery code, a challenge token, an enrolment token, a password,
// an access or refresh token) may appear anywhere in the captured output or in any audit row's metadata.

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
)

type hygieneExported struct {
	VerifyReq   mfaVerifyRequest
	RecoverReq  mfaRecoverRequest
	ConfirmReq  enrolConfirmRequest
	RedeemReq   enrolRedeemRequest
	Challenge   staffChallenge
	Pair        staffTokenPair
	Start       enrolStartResponse
	Confirm     enrolConfirmResponse
	Token       staffmfa.Token
	Material    staffmfa.EnrolmentMaterial
	Batch       staffmfa.RecoveryCodeBatch
	Assertion   staffmfa.Assertion
	Code        staffmfa.RecoveryCode
	Keys        *staffmfa.Keys
	Slice       []staffChallenge
	Map         map[string]staffTokenPair
	Interface   any
	PtrResponse *enrolConfirmResponse
}

type hygieneUnexported struct {
	verifyReq  mfaVerifyRequest
	recoverReq mfaRecoverRequest
	confirmReq enrolConfirmRequest
	redeemReq  enrolRedeemRequest
	challenge  staffChallenge
	pair       staffTokenPair
	start      enrolStartResponse
	confirm    enrolConfirmResponse
	token      staffmfa.Token
	material   staffmfa.EnrolmentMaterial
	batch      staffmfa.RecoveryCodeBatch
	assertion  staffmfa.Assertion
	keys       *staffmfa.Keys
	slice      []enrolStartResponse
	nested     struct{ inner hygieneExported }
}

func TestStaffMFA_SecretHygiene_LogCapture(t *testing.T) {
	w := newMFAW(t)
	// Capture slog.Default too (the alerting package logs through it).
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var secrets []string
	add := func(v ...string) { secrets = append(secrets, v...) }
	s := w.staff(&w.tenant, "tenant_admin")
	add(s.password)

	// Enrolment: redeem (wrong password first), start, a wrong confirm code, confirm.
	bootstrap := w.bootstrapToken(s)
	add(bootstrap)
	w.post("/v1/staff/auth/enrol/redeem", map[string]string{"token": bootstrap, "tenant_slug": s.slug, "email": s.email, "password": "wrong-" + s.password}).
		want(t, http.StatusUnauthorized, "invalid_enrolment_token")
	rd := w.redeem(s, bootstrap)
	enrolCh := challengeOf(t, rd, "enrol")
	add(enrolCh)
	sr := w.post("/v1/staff/auth/enrol/start", map[string]string{"challenge_token": enrolCh})
	var start enrolStartWire
	sr.decode(t, &start)
	add(start.SecretBase32, start.OTPAuthURI)
	w.secrets[s.ref.StaffID] = mustB32(t, start.SecretBase32)
	wrong := w.wrong(s)
	add(wrong)
	w.post("/v1/staff/auth/enrol/confirm", map[string]string{"challenge_token": enrolCh, "factor_id": start.FactorID, "code": wrong}).want(t, http.StatusUnauthorized, "invalid_mfa_code")
	code := w.code(s)
	add(code)
	cr := w.post("/v1/staff/auth/enrol/confirm", map[string]string{"challenge_token": enrolCh, "factor_id": start.FactorID, "code": code})
	cr.want(t, http.StatusOK, "")
	var conf enrolConfirmWire
	cr.decode(t, &conf)
	add(conf.RecoveryCodes...)
	add(conf.TokenPair.AccessToken, conf.TokenPair.RefreshToken)
	// Login: a wrong code, then the right one.
	ch := w.login(s)
	add(ch)
	wrong2 := w.wrong(s)
	add(wrong2)
	w.verify(ch, wrong2).want(t, http.StatusUnauthorized, "invalid_mfa_code")
	code2 := w.code(s)
	add(code2)
	vr := w.verify(ch, code2)
	var pair tokenPairResponse
	vr.decode(t, &pair)
	add(pair.AccessToken, pair.RefreshToken)
	// Recovery: a wrong code, a right one, re-enrolment.
	ch2 := w.login(s)
	add(ch2)
	w.post("/v1/staff/auth/mfa/recover", map[string]string{"challenge_token": ch2, "recovery_code": "ZZZZZ-ZZZZZ"}).want(t, http.StatusUnauthorized, "invalid_mfa_code")
	rr := w.post("/v1/staff/auth/mfa/recover", map[string]string{"challenge_token": ch2, "recovery_code": conf.RecoveryCodes[3]})
	enrol2 := challengeOf(t, rr, "enrol")
	add(enrol2)
	e2 := w.enrolWith(s, enrol2)
	add(e2.start.SecretBase32, e2.start.OTPAuthURI)
	add(e2.codes...)
	add(e2.pair.AccessToken, e2.pair.RefreshToken)
	// Refresh, a reused refresh token, logout, My security.
	rf := w.refresh(e2.pair.RefreshToken)
	var p3 tokenPairResponse
	rf.decode(t, &p3)
	add(p3.AccessToken, p3.RefreshToken)
	w.refresh(e2.pair.RefreshToken).want(t, http.StatusUnauthorized, "unauthorized")
	w.do(http.MethodGet, "/v1/staff/me/mfa", p3.AccessToken, nil).want(t, http.StatusOK, "")
	w.post("/v1/auth/logout", map[string]string{"refresh_token": p3.RefreshToken}).want(t, http.StatusNoContent, "")
	serverLog := w.logs.String() // everything the server (and slog.Default) logged during the flows

	// Domain and wire values that hold secrets, built from real material.
	var mat staffmfa.EnrolmentMaterial
	w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, mat, err = w.m.factor.Begin(ctx, tx, s.ref, mustNow(ctx, t, tx))
		return err
	})
	uri, b32 := mat.RevealForEnrolment()
	add(uri, b32)
	tok, err := staffmfa.NewToken(s.ref.StaffID)
	if err != nil {
		t.Fatal(err)
	}
	add(tok.RevealForDelivery())
	batch, err := staffmfa.GenerateRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range batch.Codes() {
		add(c.RevealForDisplay())
	}
	var verifyReq mfaVerifyRequest
	var recoverReq mfaRecoverRequest
	var confirmReq enrolConfirmRequest
	var redeemReq enrolRedeemRequest
	hygieneJSON(t, fmt.Sprintf(`{"challenge_token":%q,"code":%q}`, ch, code2), &verifyReq)
	hygieneJSON(t, fmt.Sprintf(`{"challenge_token":%q,"recovery_code":%q}`, ch2, conf.RecoveryCodes[4]), &recoverReq)
	hygieneJSON(t, fmt.Sprintf(`{"challenge_token":%q,"factor_id":%q,"code":%q}`, enrolCh, start.FactorID, code), &confirmReq)
	hygieneJSON(t, fmt.Sprintf(`{"token":%q,"tenant_slug":%q,"email":%q,"password":%q}`, bootstrap, s.slug, s.email, s.password), &redeemReq)
	chal := newStaffChallenge("mfa", tok, 300, mfaChallengeMethods)
	sp := staffTokenPair{AccessToken: newRedacted(p3.AccessToken), RefreshToken: newRedacted(p3.RefreshToken), ExpiresIn: 900}
	startResp := newEnrolStartResponse(uuid.New(), mat)
	confResp := newEnrolConfirmResponse(batch, sp)
	rc, err := staffmfa.NormaliseRecoveryCode(conf.RecoveryCodes[5])
	if err != nil {
		t.Fatal(err)
	}
	exp := hygieneExported{VerifyReq: verifyReq, RecoverReq: recoverReq, ConfirmReq: confirmReq, RedeemReq: redeemReq, Challenge: chal, Pair: sp,
		Start: startResp, Confirm: confResp, Token: tok, Material: mat, Batch: batch, Assertion: staffmfa.NewTOTPAssertion(code2), Code: rc,
		Keys: testStaffMFAKeys(), Slice: []staffChallenge{chal}, Map: map[string]staffTokenPair{"p": sp}, Interface: confResp, PtrResponse: &confResp}
	unexp := hygieneUnexported{verifyReq: verifyReq, recoverReq: recoverReq, confirmReq: confirmReq, redeemReq: redeemReq, challenge: chal, pair: sp,
		start: startResp, confirm: confResp, token: tok, material: mat, batch: batch, assertion: staffmfa.NewTOTPAssertion(code2), keys: testStaffMFAKeys(),
		slice: []enrolStartResponse{startResp}}
	unexp.nested.inner = exp

	var out bytes.Buffer
	textLog := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	jsonLog := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, l := range []*slog.Logger{textLog, jsonLog, slog.Default()} {
		l.Info("hygiene", "exported", exp, "unexported", unexp, "ptr", &unexp, "verify_req", verifyReq, "challenge", chal, "pair", sp,
			"start", startResp, "confirm", confResp, "token", tok, "material", mat,
			// bare redacted[T] values go through slog's LogValuer path, not fmt
			"code", verifyReq.Code, "challenge_token", verifyReq.ChallengeToken, "access_token", sp.AccessToken,
			"refresh_token", sp.RefreshToken, "password", redeemReq.Password, slog.Group("g", "code", confirmReq.Code))
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		fmt.Fprintf(&out, verb+"\n", exp)
		fmt.Fprintf(&out, verb+"\n", unexp)
		fmt.Fprintf(&out, verb+"\n", &unexp)
		fmt.Fprintf(&out, verb+"\n", []any{verifyReq, recoverReq, confirmReq, redeemReq, chal, sp, startResp, confResp})
	}
	for _, v := range []any{exp, unexp, &unexp, verifyReq, chal, sp, startResp, confResp} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json: %v", err)
		}
		out.Write(b)
	}

	// Non-vacuity: the secrets were really used and the capture really recorded the flows.
	if len(secrets) < 40 || !strings.Contains(serverLog, "/v1/staff/auth/") {
		t.Fatalf("vacuous hygiene run: %d secrets, %d bytes of server log", len(secrets), len(serverLog))
	}
	if !strings.Contains(out.String(), staffmfa.Redacted) {
		t.Fatal("the explicit formatting produced no redaction marker (vacuous)")
	}
	all := w.logs.String() + "\n" + out.String()
	if leaked := containsAny(all, secrets); len(leaked) > 0 {
		t.Fatalf("%d secret value(s) leaked into logs / formatted output (first: %.12s...)", len(leaked), leaked[0])
	}
	// Audit metadata never carries a secret either.
	for _, m := range w.auditMetadata(s) {
		if leaked := containsAny(m, secrets); len(leaked) > 0 {
			t.Fatalf("an audit row carries a secret: %s", strings.SplitN(m, " ", 2)[0])
		}
	}
	// And nothing staff-MFA-shaped is logged at all by the server during the flows (the request logger logs no
	// bodies; ADR 0119 section 5.4).
	for _, body := range []string{"challenge_token", "recovery_code", "secret_base32", "otpauth"} {
		if strings.Contains(serverLog, body) {
			t.Fatalf("the server log mentions %q: request or response bodies are being logged", body)
		}
	}
}

func hygieneJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
}

func mustB32(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustNow(ctx context.Context, t *testing.T, tx pgx.Tx) (now time.Time) {
	t.Helper()
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

var _ = store.ChallengeMFA
