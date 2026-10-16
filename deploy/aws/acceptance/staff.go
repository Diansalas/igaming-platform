//go:build stagingacceptance

package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// actorSpec is one staff identity the suite signs in as. Secrets are referenced by FILE PATH only.
type actorSpec struct {
	Role          string // ADMIN, ADMIN2, TENANT_ADMIN, FINANCE1, FINANCE2 (the env prefix)
	Email         string
	TenantSlug    string // "" for platform staff
	PasswordFile  string
	TOTPFile      string // 0600; base32 secret. Created on first enrolment when BootstrapFile is given.
	BootstrapFile string // 0600; single-use bootstrap/invite enrolment token (cmd/seed-admin prints it once)
}

func (s actorSpec) configured() bool { return s.Email != "" && s.PasswordFile != "" }

type actor struct {
	spec     actorSpec
	w        *world
	password string
	totp     *totpSecret
	access   string
	refresh  string
	loginAt  time.Time
	tokenAt  time.Time // when the current access token was issued (staff access tokens live 15 minutes)
	stepUpAt time.Time // when the last step-up (or login/enrolment) established a fresh second factor
	enrolled bool      // this run enrolled the factor
	err      error     // sticky failure of the sign-in (reported once by the check that tried)
}

// readSecretFile reads a secret from a regular file that is not accessible to group/other. The content is returned
// trimmed and registered with the scrubber by the caller.
func readSecretFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %v", baseName(path), pathErr(err))
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", baseName(path))
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s has mode %04o: refuse to read a secret file accessible to group/other (chmod 600)", baseName(path), st.Mode().Perm())
	}
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied secret file path, mode-checked above.
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %v", baseName(path), pathErr(err))
	}
	return strings.TrimSpace(string(b)), nil
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func pathErr(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// load reads the password and, when present, the TOTP secret (and its last-used-step sidecar).
func (a *actor) load() error {
	pw, err := readSecretFile(a.spec.PasswordFile)
	if err != nil {
		return fmt.Errorf("%s password: %w", a.spec.Role, err)
	}
	if pw == "" {
		return fmt.Errorf("%s password file is empty", a.spec.Role)
	}
	a.password = pw
	a.w.scrub.add(pw)
	if a.spec.TOTPFile != "" {
		if st, err := os.Stat(a.spec.TOTPFile); err == nil && st.Size() > 0 {
			s, err := readSecretFile(a.spec.TOTPFile)
			if err != nil {
				return fmt.Errorf("%s totp: %w", a.spec.Role, err)
			}
			sec, err := newTOTPSecretFromBase32(s)
			if err != nil {
				return fmt.Errorf("%s totp: %w", a.spec.Role, err)
			}
			a.totp = sec
			a.w.scrub.add(s)
			a.w.scrub.add(sec.base32())
			a.attachPersist()
			if b, err := os.ReadFile(a.spec.TOTPFile + ".laststep"); err == nil { //nolint:gosec // sidecar next to the operator's file
				if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
					a.totp.lastUsed = n
				}
			}
		}
	}
	return nil
}

func (a *actor) attachPersist() {
	if a.spec.TOTPFile == "" || a.totp == nil {
		return
	}
	path := a.spec.TOTPFile + ".laststep"
	a.totp.persist = func(step int64) {
		_ = os.WriteFile(path, []byte(strconv.FormatInt(step, 10)+"\n"), 0o600)
	}
}

func (a *actor) code() string {
	return a.totp.next(a.w.now, a.w.sleep)
}

func (a *actor) loginBody() map[string]any {
	b := map[string]any{"email": a.spec.Email, "password": a.password}
	if a.spec.TenantSlug != "" {
		b["tenant_slug"] = a.spec.TenantSlug
	}
	return b
}

// signIn performs the password step and then either the MFA verification or (first use, bootstrap token present)
// the enrolment. It never prints a secret. On success a.access is an aal2 staff access token.
func (a *actor) signIn() error {
	if a.access != "" && a.w.now().Sub(a.tokenAt) > 12*time.Minute {
		a.access, a.refresh = "", "" // the access token is about to expire (15 min): sign in again
	}
	if a.access != "" {
		return nil
	}
	if a.err != nil {
		return a.err
	}
	fail := func(format string, args ...any) error {
		a.err = fmt.Errorf(format, args...)
		return a.err
	}
	if a.password == "" {
		if err := a.load(); err != nil {
			return fail("%v", err)
		}
	}
	c := a.w.c
	r, err := c.call("POST", "/v1/staff/auth/login", "", a.loginBody())
	if err != nil {
		return fail("%s login: %v", a.spec.Role, err)
	}
	switch {
	case r.Status == 200 && pickStr(r.obj(), "challenge", "token") != "":
		ch := pickStr(r.obj(), "challenge", "token")
		a.w.scrub.add(ch)
		if !a.totp.valid() {
			return fail("%s: the account has an enrolled second factor but no TOTP secret file was supplied (%s_TOTP_SECRET_FILE)", a.spec.Role, a.spec.Role)
		}
		return a.verify(ch)
	case r.Status == 403 && r.code() == "mfa_enrolment_required":
		if a.spec.BootstrapFile == "" {
			return fail("%s: no second factor is enrolled and no bootstrap token file was supplied (%s_BOOTSTRAP_TOKEN_FILE)", a.spec.Role, a.spec.Role)
		}
		return a.enrol()
	default:
		return fail("%s login refused: %s", a.spec.Role, r.brief())
	}
}

func (a *actor) verify(challenge string) error {
	r, err := a.w.c.call("POST", "/v1/staff/auth/mfa/verify", "", map[string]any{"challenge_token": challenge, "code": a.code()})
	if err != nil {
		a.err = fmt.Errorf("%s mfa verify: %v", a.spec.Role, err)
		return a.err
	}
	if r.Status != 200 {
		a.err = fmt.Errorf("%s mfa verify refused: %s", a.spec.Role, r.brief())
		return a.err
	}
	return a.takeTokens(r)
}

func (a *actor) takeTokens(r *response) error {
	m := r.obj()
	tp := m
	if t, ok := m["token_pair"].(map[string]any); ok {
		tp = t
	}
	a.access = pickStr(tp, "access_token")
	a.refresh = pickStr(tp, "refresh_token")
	if a.access == "" {
		a.err = fmt.Errorf("%s: response carried no access token (%s)", a.spec.Role, r.brief())
		return a.err
	}
	a.w.scrub.add(a.access)
	a.w.scrub.add(a.refresh)
	a.loginAt = a.w.now()
	a.tokenAt = a.loginAt
	a.stepUpAt = a.loginAt
	return nil
}

// enrol redeems the bootstrap token and enrols a TOTP factor. The shared secret exists only in memory and in the
// operator-designated 0600 file; it is written BEFORE the factor is confirmed so a crash can never leave an enrolled
// account whose secret was lost. Recovery codes go to a sibling 0600 file and are never printed.
func (a *actor) enrol() error {
	c := a.w.c
	if a.spec.TOTPFile == "" {
		a.err = fmt.Errorf("%s: enrolment needs %s_TOTP_SECRET_FILE (where the new TOTP secret is stored 0600); refusing to enrol into memory only", a.spec.Role, a.spec.Role)
		return a.err
	}
	if st, err := os.Stat(a.spec.TOTPFile); err == nil && st.Size() > 0 {
		a.err = fmt.Errorf("%s: TOTP secret file already has content but the server has no active factor; not overwriting (remove it deliberately)", a.spec.Role)
		return a.err
	}
	tok, err := readSecretFile(a.spec.BootstrapFile)
	if err != nil {
		a.err = fmt.Errorf("%s bootstrap token: %w", a.spec.Role, err)
		return a.err
	}
	a.w.scrub.add(tok)
	body := map[string]any{"token": tok, "email": a.spec.Email, "password": a.password}
	if a.spec.TenantSlug != "" {
		body["tenant_slug"] = a.spec.TenantSlug
	}
	r, err := c.call("POST", "/v1/staff/auth/enrol/redeem", "", body)
	if err != nil {
		a.err = fmt.Errorf("%s enrol redeem: %v", a.spec.Role, err)
		return a.err
	}
	ch := pickStr(r.obj(), "challenge", "token")
	if r.Status != 200 || ch == "" {
		a.err = fmt.Errorf("%s enrol redeem refused (token used, expired or credentials differ): %s", a.spec.Role, r.brief())
		return a.err
	}
	a.w.scrub.add(ch)
	r, err = c.call("POST", "/v1/staff/auth/enrol/start", "", map[string]any{"challenge_token": ch})
	if err != nil || r.Status != 200 {
		if err == nil {
			err = fmt.Errorf("%s", r.brief())
		}
		a.err = fmt.Errorf("%s enrol start: %v", a.spec.Role, err)
		return a.err
	}
	m := r.obj()
	secB32, factorID := pickStr(m, "secret_base32"), pickStr(m, "factor_id")
	a.w.scrub.add(secB32)
	a.w.scrub.add(pickStr(m, "otpauth_uri"))
	sec, err := newTOTPSecretFromBase32(secB32)
	if err != nil || factorID == "" {
		a.err = fmt.Errorf("%s enrol start returned no usable TOTP secret", a.spec.Role)
		return a.err
	}
	a.w.scrub.add(sec.base32())
	// Persist first (O_EXCL when new; the operator may have pre-created an empty 0600 file).
	if err := writeSecretFile(a.spec.TOTPFile, sec.base32()+"\n"); err != nil {
		a.err = fmt.Errorf("%s: cannot store the TOTP secret (%v); NOT confirming the factor", a.spec.Role, pathErr(err))
		return a.err
	}
	a.totp = sec
	a.attachPersist()
	r, err = c.call("POST", "/v1/staff/auth/enrol/confirm", "", map[string]any{"challenge_token": ch, "factor_id": factorID, "code": a.code()})
	if err != nil || r.Status != 200 {
		if err == nil {
			err = fmt.Errorf("%s", r.brief())
		}
		_ = os.Remove(a.spec.TOTPFile) // the pending factor never became active; do not leave a dead secret behind
		a.totp = nil
		a.err = fmt.Errorf("%s enrol confirm failed: %v", a.spec.Role, err)
		return a.err
	}
	codes, _ := pick(r.obj(), "recovery_codes").([]any)
	var sb strings.Builder
	for _, c := range codes {
		if s, ok := c.(string); ok {
			a.w.scrub.add(s)
			sb.WriteString(s + "\n")
		}
	}
	if sb.Len() > 0 {
		if err := writeSecretFile(a.spec.TOTPFile+".recovery-codes", sb.String()); err != nil {
			a.w.warnf("%s: recovery codes could NOT be stored (%v) and are lost; reset MFA before relying on this account", a.spec.Role, pathErr(err))
		}
	}
	a.enrolled = true
	return a.takeTokens(r)
}

func writeSecretFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // operator-designated secret path
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// stepUp performs POST /v1/staff/auth/step-up and swaps in the fresh access token.
func (a *actor) stepUp() error {
	if err := a.signIn(); err != nil {
		return err
	}
	if !a.totp.valid() {
		return fmt.Errorf("%s: no TOTP secret", a.spec.Role)
	}
	r, err := a.w.c.call("POST", "/v1/staff/auth/step-up", a.access, map[string]any{"code": a.code()})
	if err != nil {
		return fmt.Errorf("%s step-up: %v", a.spec.Role, err)
	}
	if r.Status != 200 {
		return fmt.Errorf("%s step-up refused: %s", a.spec.Role, r.brief())
	}
	tok := pickStr(r.obj(), "access_token")
	if tok == "" {
		return fmt.Errorf("%s step-up returned no access token", a.spec.Role)
	}
	a.w.scrub.add(tok)
	a.access = tok
	a.stepUpAt = a.w.now()
	a.tokenAt = a.stepUpAt
	return nil
}

// fresh makes sure a second factor was presented recently enough for the next critical/financial action. maxAge is
// kept well inside the server windows (critical 5 min, financial 10 min).
func (a *actor) fresh(maxAge time.Duration) error {
	if err := a.signIn(); err != nil {
		return err
	}
	if a.w.now().Sub(a.stepUpAt) < maxAge {
		return nil
	}
	return a.stepUp()
}

// freshCritical / freshFinancial are the two classes the suite uses.
func (a *actor) freshCritical() error  { return a.fresh(3 * time.Minute) }
func (a *actor) freshFinancial() error { return a.fresh(6 * time.Minute) }

func (a *actor) call(method, path string, body any) (*response, error) {
	if err := a.signIn(); err != nil {
		return nil, err
	}
	return a.w.c.call(method, path, a.access, body)
}
