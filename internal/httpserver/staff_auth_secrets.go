package httpserver

// Secret-bearing request and response types of the ADR 0119 S3 staff
// authentication endpoints (section 5.4 and S3 entry gate (f)). Every secret a
// request carries (challenge tokens, codes, recovery codes, enrolment tokens,
// passwords) and every secret a response carries (challenge tokens, the TOTP
// secret and otpauth URI, recovery codes, access and refresh tokens) is held
// in a redacted[T]: the value sits behind an unexported closure, so fmt, slog
// and encoding/json render "[REDACTED]" for it whether it is printed directly,
// held in an exported field (the methods below) or held in an unexported field
// of another struct (fmt then prints the closure's address, never the value).
// The ONLY places a raw value leaves these types are the wire() methods, which
// build a throwaway plain struct that writeStaffSecretJSON marshals straight to
// the response body - and the staffmfa RevealFor* calls in the constructors
// below, which a static test pins as the only production RevealFor* callers
// outside cmd/seed-admin (S3 entry gate G5).

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// redacted holds one secret value behind an unexported closure.
type redacted[T any] struct{ f func() T }

func newRedacted[T any](v T) redacted[T] { return redacted[T]{f: func() T { return v }} }

// reveal returns the raw value (the zero value when unset).
func (r redacted[T]) reveal() T {
	if r.f == nil {
		var zero T
		return zero
	}
	return r.f()
}

func (redacted[T]) String() string                  { return staffmfa.Redacted }
func (redacted[T]) GoString() string                { return staffmfa.Redacted }
func (redacted[T]) Format(f fmt.State, _ rune)      { _, _ = f.Write([]byte(staffmfa.Redacted)) }
func (redacted[T]) LogValue() slog.Value            { return slog.StringValue(staffmfa.Redacted) }
func (redacted[T]) MarshalJSON() ([]byte, error)    { return json.Marshal(staffmfa.Redacted) }
func (r *redacted[T]) UnmarshalJSON(b []byte) error { return r.unmarshal(b) }

func (r *redacted[T]) unmarshal(b []byte) error {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*r = newRedacted(v)
	return nil
}

// ---- requests -----------------------------------------------------------------

type mfaVerifyRequest struct {
	ChallengeToken redacted[string] `json:"challenge_token"`
	Code           redacted[string] `json:"code"`
}

type mfaRecoverRequest struct {
	ChallengeToken redacted[string] `json:"challenge_token"`
	RecoveryCode   redacted[string] `json:"recovery_code"`
}

type enrolStartRequest struct {
	ChallengeToken redacted[string] `json:"challenge_token"`
}

type enrolConfirmRequest struct {
	ChallengeToken redacted[string] `json:"challenge_token"`
	FactorID       string           `json:"factor_id"`
	Code           redacted[string] `json:"code"`
}

type enrolRedeemRequest struct {
	Token      redacted[string] `json:"token"`
	TenantSlug string           `json:"tenant_slug,omitempty"`
	Email      string           `json:"email"`
	Password   redacted[string] `json:"password"`
}

// ---- responses ----------------------------------------------------------------

// secretResponse is a response whose body carries a secret. It is written only
// through writeStaffSecretJSON.
type secretResponse interface {
	wire() any
}

// writeStaffSecretJSON marshals resp.wire() (a throwaway plain struct) to the
// body. The caller has already set the no-store headers.
func writeStaffSecretJSON(w http.ResponseWriter, status int, resp secretResponse) {
	writeJSON(w, status, resp.wire())
}

// staffChallenge is the {"challenge": {...}} body of login, recover and redeem.
type staffChallenge struct {
	Type      string
	Token     redacted[string]
	ExpiresIn int
	Methods   []string
}

func newStaffChallenge(typ string, tok staffmfa.Token, expiresIn int, methods []string) staffChallenge {
	return staffChallenge{Type: typ, Token: newRedacted(tok.RevealForDelivery()), ExpiresIn: expiresIn,
		Methods: append([]string(nil), methods...)}
}

type staffChallengeWire struct {
	Challenge staffChallengeWireBody `json:"challenge"`
}

type staffChallengeWireBody struct {
	Type      string   `json:"type"`
	Token     string   `json:"token"`
	ExpiresIn int      `json:"expires_in"`
	Methods   []string `json:"methods"`
}

func (c staffChallenge) wire() any {
	return staffChallengeWire{Challenge: staffChallengeWireBody{Type: c.Type, Token: c.Token.reveal(), ExpiresIn: c.ExpiresIn, Methods: c.Methods}}
}

// staffTokenPair is an aal2 staff token pair.
type staffTokenPair struct {
	AccessToken  redacted[string]
	RefreshToken redacted[string]
	ExpiresIn    int
}

func (p staffTokenPair) plain() tokenPairResponse {
	return tokenPairResponse{AccessToken: p.AccessToken.reveal(), RefreshToken: p.RefreshToken.reveal(), TokenType: "Bearer", ExpiresIn: p.ExpiresIn}
}

func (p staffTokenPair) wire() any { return p.plain() }

// enrolStartResponse carries the TOTP secret exactly once.
type enrolStartResponse struct {
	FactorID     uuid.UUID
	OTPAuthURI   redacted[string]
	SecretBase32 redacted[string]
	ExpiresIn    int
}

func newEnrolStartResponse(id uuid.UUID, m staffmfa.EnrolmentMaterial) enrolStartResponse {
	uri, secret := m.RevealForEnrolment()
	return enrolStartResponse{FactorID: id, OTPAuthURI: newRedacted(uri), SecretBase32: newRedacted(secret),
		ExpiresIn: int(staffmfa.PendingFactorTTL.Seconds())}
}

type enrolStartWire struct {
	FactorID     string `json:"factor_id"`
	OTPAuthURI   string `json:"otpauth_uri"`
	SecretBase32 string `json:"secret_base32"`
	ExpiresIn    int    `json:"expires_in"`
}

func (r enrolStartResponse) wire() any {
	return enrolStartWire{FactorID: r.FactorID.String(), OTPAuthURI: r.OTPAuthURI.reveal(), SecretBase32: r.SecretBase32.reveal(), ExpiresIn: r.ExpiresIn}
}

// enrolConfirmResponse carries the recovery codes and the token pair exactly once.
type enrolConfirmResponse struct {
	RecoveryCodes redacted[[]string]
	TokenPair     staffTokenPair
}

func newEnrolConfirmResponse(batch staffmfa.RecoveryCodeBatch, pair staffTokenPair) enrolConfirmResponse {
	codes := batch.Codes()
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		out = append(out, c.RevealForDisplay())
	}
	return enrolConfirmResponse{RecoveryCodes: newRedacted(out), TokenPair: pair}
}

type enrolConfirmWire struct {
	RecoveryCodes []string          `json:"recovery_codes"`
	TokenPair     tokenPairResponse `json:"token_pair"`
}

func (r enrolConfirmResponse) wire() any {
	return enrolConfirmWire{RecoveryCodes: r.RecoveryCodes.reveal(), TokenPair: r.TokenPair.plain()}
}

// staffMFAStatusResponse is GET /v1/staff/me/mfa (no secret).
type staffMFAStatusResponse struct {
	Enrolled               bool    `json:"enrolled"`
	FactorType             *string `json:"factor_type"`
	ActivatedAt            *string `json:"activated_at"`
	RecoveryCodesRemaining int     `json:"recovery_codes_remaining"`
	HardLocked             bool    `json:"hard_locked"`
	LockedUntil            *string `json:"locked_until"`
}
