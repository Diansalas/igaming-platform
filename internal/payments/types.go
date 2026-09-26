// Package payments implements the payment-orchestration layer: the
// PaymentProvider adapter interface, the PaymentOrchestrator that routes
// deposits to a provider and translates provider callbacks into ledger
// postings, the ProviderCapability configuration model, and a MOCK PSP
// adapter for development/testing.
//
// Scope for this stage (Stage 3B): deposits only (InitiateDeposit,
// RouteProvider, ReceiveCallback), currency/asset -> payment method ->
// amount -> provider-health routing dimensions, cascade-on-decline, and
// ambiguous-outcome handling. Withdrawal orchestration
// (InitiateWithdrawalRequest) is NOT IMPLEMENTED here - it belongs to the
// withdrawal-state-machine work, which calls PaymentProvider.Withdraw only
// on its own approved->submitted transition
// (docs/architecture/payment-orchestration.md §3). Crypto rails and
// CryptoCustodyProvider are entirely out of scope (docs/decisions/0022
// §4) - nothing in this package imports or references custodian/key-
// management code.
//
// See docs/architecture/payment-orchestration.md and
// docs/decisions/0022-payment-provider-agnosticism-and-capability-model.md
// for the frozen design this package implements against.
package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Outcome is the canonical result state shared by DepositResult,
// WithdrawResult, StatusResult, and CallbackEvent. The distinction between
// OutcomeDeclined (a definite negative response) and OutcomeAmbiguous
// (timeout/unknown/no callback) is load-bearing:
// payment-orchestration.md §5 forbids cascading on an ambiguous outcome
// without first calling QueryStatus, because cascading on what might
// actually be a delayed success risks charging a player twice through two
// independent DepositIntent attempts. No adapter may collapse these into
// one generic "not success" value (docs/decisions/0022 §6(a)).
type Outcome string

const (
	// OutcomePending means the operation is underway asynchronously (e.g.
	// a hosted redirect flow awaiting the player to complete payment) and
	// a later callback or QueryStatus call is expected to resolve it.
	OutcomePending Outcome = "pending"
	// OutcomeSucceeded is a definite positive result.
	OutcomeSucceeded Outcome = "succeeded"
	// OutcomeDeclined is a definite negative result. Cascade-eligibility
	// is carried separately (Cascadable), not implied by this value alone.
	OutcomeDeclined Outcome = "declined"
	// OutcomeAmbiguous is a timeout, missing callback, or otherwise
	// unresolved network/provider outcome - never treated as a decline
	// and never cascaded without a QueryStatus call first.
	OutcomeAmbiguous Outcome = "ambiguous"
)

// ProviderKind mirrors provider_capabilities.provider_kind
// (docs/decisions/0022 §2). Not tenant-editable - see WriteCapability.
type ProviderKind string

const (
	ProviderKindFiat          ProviderKind = "fiat"
	ProviderKindCryptoPayment ProviderKind = "crypto_payment"
)

// CallbackCapability mirrors provider_capabilities.callback_capabilities.
type CallbackCapability string

const (
	CallbackWebhookOnly CallbackCapability = "webhook"
	CallbackPollingOnly CallbackCapability = "polling_only"
	CallbackBoth        CallbackCapability = "both"
)

// CapabilityStatus mirrors provider_capabilities.status - an operator
// kill-switch independent of health/circuit-breaker state.
type CapabilityStatus string

const (
	CapabilityActive   CapabilityStatus = "active"
	CapabilityDisabled CapabilityStatus = "disabled"
)

// CircuitState mirrors payment-orchestration.md §6's ProviderHealth
// circuit_state.
type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"
	CircuitOpen     CircuitState = "open"
	CircuitHalfOpen CircuitState = "half_open"
)

// Sentinel errors.
var (
	// ErrInboundKeyMaterial is returned by a PaymentProvider adapter (and
	// must be returned, never silently swallowed) when a provider response
	// or webhook carries what looks like a private key, seed/mnemonic,
	// spend-capable extended key, or signing handle
	// (docs/decisions/0022 §4.1). The offending value is never included in
	// the error string, never logged, never persisted.
	ErrInboundKeyMaterial = errors.New("payments: inbound payload rejected: contains apparent key material")

	// ErrCallbackSignatureInvalid is returned by a PaymentProvider's
	// HandleCallback when the payload's authentication (signature, MAC,
	// or whatever mechanism that adapter's vendor uses) does not verify.
	// A verified callback is the ONLY thing standing between the
	// unauthenticated webhook route (POST /v1/webhooks/payments/
	// {tenantSlug}/{providerID} carries no bearer token, since a provider
	// webhook isn't an authenticated platform principal) and posting to
	// the ledger - see payment-orchestration.md §3: "An unverified payload
	// never reaches the ledger posting API." Never wrap this with the raw
	// payload or any field from it.
	ErrCallbackSignatureInvalid = errors.New("payments: callback signature verification failed")
	// ErrCallbackMalformedBody is returned by a PaymentProvider adapter's
	// HandleCallback for a structural parsing failure discovered AFTER
	// signature verification has already succeeded (docs/decisions/0022
	// §3 amendment point 7, added Stage 10.1 security review P2-1/code
	// review F1/architect PW-1): an unparseable JSON body, a missing
	// required field, or an unrecognized event_type/outcome value, from a
	// caller who has already proven knowledge of the shared credential. It
	// is deliberately a DIFFERENT sentinel from ErrCallbackSignatureInvalid
	// - a verified-but-malformed body maps to a 4xx validation response,
	// never to the uniform pre-verification 401, and is safe to describe
	// more specifically in logs/responses since the sender is
	// authenticated. Never returned for a failure that occurs BEFORE
	// verification succeeds - see HandleCallback's own doc comment.
	ErrCallbackMalformedBody = errors.New("payments: verified callback body is malformed")
	// ErrUnknownProvider is returned when a capability row or routing
	// decision names a provider_id the orchestrator has no adapter
	// registered for.
	ErrUnknownProvider = errors.New("payments: unknown provider_id")
	// ErrNoRoutableProvider is returned by RouteProvider when no
	// candidate survives every routing dimension.
	ErrNoRoutableProvider = errors.New("payments: no routable provider for this request")
	// ErrCapabilityWidensAdapter is returned by WriteCapability when a
	// tenant-configured row would assert more than the adapter's own
	// declared capability (docs/decisions/0022 §2.1: "may only narrow...
	// never widen").
	ErrCapabilityWidensAdapter = errors.New("payments: capability configuration widens beyond the adapter's declared capability")
	// ErrDepositIntentNotFound is returned when a callback's
	// (provider_id, provider_reference) pair matches no deposit_intents
	// row visible in the current tenant scope.
	ErrDepositIntentNotFound = errors.New("payments: no matching deposit intent for this provider reference")
	// ErrCallbackProviderMismatch is returned when a verified callback's
	// declared facts (amount/asset) do not match the deposit intent it
	// claims to resolve.
	ErrCallbackProviderMismatch = errors.New("payments: callback event does not match the resolved deposit intent")
	// ErrCallbackPayloadMismatch is returned when a callback reuses a
	// provider reference that is already posted to the ledger, but the
	// posting it would make differs from the stored one (Stage 10 F-7
	// remediation, ADR 0020 amendment 2026-09-25; wraps
	// ledger.ErrIdempotencyPayloadMismatch). The motivating case (audit
	// site #20): a reversal reference R1, already used to reverse deposit
	// D1, redelivered naming a different deposit D2 - which used to return
	// success reporting D2 reversed while D2 was never debited. Nothing is
	// posted; it is an integrity alert, never a retry signal.
	ErrCallbackPayloadMismatch = errors.New("payments: provider reference already posted with a different payload")
	// ErrOriginalTransactionNotFound is returned when a deposit-reversal
	// callback's original reference matches a deposit_intents row that
	// was never actually posted to the ledger.
	ErrOriginalTransactionNotFound = errors.New("payments: no posted ledger transaction for the original deposit reference")
	// ErrDepositAlreadyReversed is returned when a deposit-reversal
	// callback names an original deposit that already has a reversal
	// posted against it. Stage 3B implements only whole-deposit reversal
	// (financial-transaction-flows.md Flow 2), never a partial-refund
	// feature, so a second reversal callback for the same original -
	// under a NEW provider_reference of its own, which the ledger's
	// (tenant_id, provider_id, provider_tx_id) uniqueness would not by
	// itself catch - must be rejected rather than posted as a second,
	// independent debit.
	ErrDepositAlreadyReversed = errors.New("payments: deposit already has a reversal posted against it")
	// ErrDepositReversalIntegrity is returned by a deposit-reversal
	// callback's Stage 10.1 PAY-REV-1 S2 step (docs/plans/
	// stage-10.1-planning-gate-proposal.md §E) when the deposit_intents
	// row it resolved names a ledger_transactions id (via
	// LedgerTransactionID) that either does not exist for this tenant, or
	// exists but is not itself a 'deposit' transaction. This is data
	// corruption, never a legitimate late/duplicate reversal - it must
	// NEVER be routed to the tombstone branch (which is for "no ledger
	// transaction was ever posted for this reference" at all, a different
	// and legitimate case) and never posts anything.
	ErrDepositReversalIntegrity = errors.New("payments: original deposit reference does not resolve to a valid deposit ledger transaction")
	// ErrIdempotencyKeyReused is returned by InitiateDeposit when a
	// retried call reuses (tenant_id, player_account_id, idempotency_key)
	// but with different deposit parameters (brand, wallet, asset, amount,
	// or payment method) than the original request - the payments-package
	// analogue of internal/ledger.ErrIdempotencyKeyReused and
	// internal/withdrawal.ErrIdempotencyKeyReused's identical rule: a
	// retried call is rejected, never silently accepted against the new
	// payload.
	ErrIdempotencyKeyReused = errors.New("payments: idempotency key reused with different deposit parameters")

	// ErrCallbackAuthFailed is the single sentinel every pre-verification
	// inbound-callback rejection wraps (PAY-WH-TENANT-1, ADR 0090 item 3;
	// docs/decisions/0022 §3 amendment 2026-09-26). Every reason gets the
	// SAME HTTP response on the public webhook route - a uniform 401
	// "callback rejected" - so an unauthenticated caller can never
	// distinguish "unknown tenant" from "bad signature" from "provider not
	// configured for this tenant" by status code or body alone. Callers
	// that need the specific reason (for allow-listed structured logging
	// only - never in the HTTP response) use errors.As to a
	// *CallbackAuthError.
	ErrCallbackAuthFailed = errors.New("payments: callback authentication failed")

	// ErrWebhookCredentialUnavailable is returned by a
	// WebhookCredentialResolver when it has no credential for the given
	// (tenantID, providerID, keyID) - folded into ErrCallbackAuthFailed's
	// "credential_unavailable" reason by the orchestrator, never surfaced
	// on its own.
	ErrWebhookCredentialUnavailable = errors.New("payments: no webhook credential available for this tenant/provider/key")
)

// CallbackAuthReason is the closed, allow-listed reason enum behind
// ErrCallbackAuthFailed (design §3.2). Logged (never returned to an
// unauthenticated caller) so operators can distinguish failure modes
// without the public response doing so.
type CallbackAuthReason string

const (
	ReasonTenantUnknown         CallbackAuthReason = "tenant_unknown"
	ReasonTenantInactive        CallbackAuthReason = "tenant_inactive"
	ReasonProviderInvalid       CallbackAuthReason = "provider_invalid"
	ReasonProviderUnregistered  CallbackAuthReason = "provider_unregistered"
	ReasonProviderNotConfigured CallbackAuthReason = "provider_not_configured"
	ReasonNoResolver            CallbackAuthReason = "no_resolver"
	ReasonCredentialUnavailable CallbackAuthReason = "credential_unavailable"
	ReasonSignatureMissing      CallbackAuthReason = "signature_missing"
	ReasonSignatureInvalid      CallbackAuthReason = "signature_invalid"
	ReasonKeyMaterial           CallbackAuthReason = "key_material"
	// ReasonBodyTooLarge is used by the HTTP handler only (never by an
	// adapter's HandleCallback) for an oversized/unreadable body, checked
	// BEFORE the tenant lookup (Stage 10.1 security review P2-1/code
	// review F1/architect PW-1, ruling 5): an oversized body must get the
	// SAME uniform 401 as every other pre-verification failure, tenant-
	// independent, rather than a distinguishable 400 that only an active,
	// resolvable tenant slug would reach.
	ReasonBodyTooLarge CallbackAuthReason = "body_too_large"
)

// CallbackAuthError is ErrCallbackAuthFailed's concrete carrier, with the
// extra, still allow-listed context (§3.3) a caller needs to log a
// payment_webhook_auth_failed line: KeyID only once it has passed the
// charset check, CredentialFingerprint only for ReasonSignatureInvalid
// (never the secret itself - WebhookCredential.Fingerprint is the only
// loggable form).
type CallbackAuthError struct {
	Reason                CallbackAuthReason
	KeyID                 string
	CredentialFingerprint string
}

func (e *CallbackAuthError) Error() string {
	return fmt.Sprintf("payments: callback authentication failed: %s", e.Reason)
}

// Is lets errors.Is(err, ErrCallbackAuthFailed) succeed for any
// *CallbackAuthError regardless of its specific Reason.
func (e *CallbackAuthError) Is(target error) bool {
	return target == ErrCallbackAuthFailed
}

// DepositAlreadyReversedError is ErrDepositAlreadyReversed's typed carrier
// (Stage 10.1 review: ledger-finance P2-B, security P2-2, code review F2).
// A bare ErrDepositAlreadyReversed left the HTTP layer with no way to
// identify WHICH deposit was affected, so the deposit.reversal_rejected
// audit record - the only durable trace of a rejected reversal, since the
// alert log line is deliberately allow-listed/generic (S-5) and the
// failed posting transaction itself was rolled back - named no deposit,
// no ledger transaction, and no PSP reference at all. Operations and PSP
// reconciliation need this to identify which real-world event (e.g. a
// refund followed by a chargeback) the rejected reversal corresponds to.
//
// Every field here has already been authenticated by the time this error
// is constructed (the callback's signature verified), so recording them
// in the tenant-scoped, append-only audit store is not an unauthenticated-
// input write - it is exactly the same trust boundary the success-path
// deposit.reversed audit record already crosses. The HTTP response body
// and the alert log line remain generic/allow-listed regardless; only the
// audit record gets this detail.
type DepositAlreadyReversedError struct {
	// DepositIntentID is the ORIGINAL deposit's deposit_intents id - the
	// audit record's TargetID.
	DepositIntentID uuid.UUID
	// OriginalLedgerTransactionID is the original deposit's posted
	// ledger_transactions id.
	OriginalLedgerTransactionID uuid.UUID
	// RejectedReversalReference is the VERIFIED (signature already
	// checked) provider_reference of the reversal callback that was
	// rejected - never the original deposit's own reference.
	RejectedReversalReference string
	// ExistingReversalTransactionID is the ALREADY-POSTED reversal's
	// ledger_transactions id, when it could be determined. Nil only if a
	// concurrent lookup could not resolve it (never expected in practice,
	// since invariant INV-PAY-REV-1 guarantees at most one exists).
	ExistingReversalTransactionID *uuid.UUID
}

func (e *DepositAlreadyReversedError) Error() string {
	return fmt.Sprintf("%s: deposit_intent=%s reversal_reference=%q",
		ErrDepositAlreadyReversed.Error(), e.DepositIntentID, e.RejectedReversalReference)
}

// Unwrap lets errors.Is(err, ErrDepositAlreadyReversed) and
// errors.As(err, &mapReceiveCallbackError's own checks) keep working
// unchanged for every existing caller that only cares about the sentinel,
// never the detail.
func (e *DepositAlreadyReversedError) Unwrap() error {
	return ErrDepositAlreadyReversed
}

// WebhookCredential is a resolved, per-(tenant, provider, key) inbound
// webhook signing credential (docs/decisions/0022 §3 amendment). Secret is
// never logged, errored, or audited - String()/LogValue() redact it, and
// Fingerprint (hex(sha256(Secret))[:16]) is the only loggable form.
type WebhookCredential struct {
	TenantID    uuid.UUID
	ProviderID  string
	KeyID       string
	Secret      []byte
	Fingerprint string
}

// String implements fmt.Stringer so %v/%+v/Println of a WebhookCredential
// (including inside a larger struct) never renders Secret's raw bytes.
func (c WebhookCredential) String() string {
	return fmt.Sprintf("WebhookCredential{TenantID:%s ProviderID:%s KeyID:%s Fingerprint:%s}",
		c.TenantID, c.ProviderID, c.KeyID, c.Fingerprint)
}

// LogValue implements slog.LogValuer for the identical reason as String.
func (c WebhookCredential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("tenant_id", c.TenantID.String()),
		slog.String("provider_id", c.ProviderID),
		slog.String("key_id", c.KeyID),
		slog.String("fingerprint", c.Fingerprint),
	)
}

// GoString implements fmt.GoStringer so `%#v` (which bypasses Stringer
// entirely, unlike %v/%+v) also never renders Secret's raw bytes -
// security review P3-1. Any struct that embeds a WebhookCredential and is
// itself %#v-formatted inherits this redaction automatically, since Go's
// fmt package calls GoString on an embedded field that implements it.
func (c WebhookCredential) GoString() string {
	return c.String()
}

// WebhookCredentialResolver resolves the single candidate credential for
// (tenantID, providerID, keyID) - never a cross-tenant trial (ADR 0022 §3
// amendment: "exactly one credential is tried"). One resolver is injected
// into the Orchestrator (C2: not a per-provider map) and is the ONLY
// component in the platform that ever sees credential secret material for
// inbound callbacks. A provider with no resolver configured fails closed
// (ReasonNoResolver) - there is no fallback to unauthenticated
// verification.
type WebhookCredentialResolver interface {
	Resolve(ctx context.Context, tenantID uuid.UUID, providerID, keyID string) (WebhookCredential, error)
}

// InboundCallback is the platform-wide inbound-provider-callback shape
// (docs/decisions/0022 §3 amendment's "binding contract", carried by
// HandleCallback in place of a bare rawPayload []byte). Header carries the
// raw request headers (including X-Payments-Signature/X-Payments-Key-Id)
// and Body the raw wire bytes - both are input only, never persisted or
// logged verbatim (ADR 0022 §4.1(3) does not apply: this is not a
// canonical output shape). TenantID/ProviderID are always the
// ROUTE-resolved values the orchestrator is verifying against, never a
// value read from Body.
type InboundCallback struct {
	TenantID   uuid.UUID
	ProviderID string
	Header     http.Header
	Body       []byte
}

// AmountLimit is one (asset_code, min_amount, max_amount) row - a
// provider declaring several assets carries one limit pair per asset,
// never a single platform-wide pair (docs/decisions/0022 §2). Amounts are
// int64 minor units at that asset's own exponent, matching
// internal/ledger's convention - never floating point.
type AmountLimit struct {
	AssetCode string
	MinAmount int64
	MaxAmount int64
}

// AdapterCapability is the adapter-declared layer only - "layer (a)" of
// docs/decisions/0022 §2's "two layers, one shape": static properties of
// the integration, returned verbatim by a PaymentProvider's Capabilities()
// method. It deliberately excludes tenant_id, brand_id, priority, and
// status, which are tenant-config data (layer (b)) an adapter never
// asserts about itself - see ProviderCapability.
type AdapterCapability struct {
	ProviderID              string
	ProviderKind            ProviderKind
	SupportedFiatCurrencies []string
	SupportedCryptoAssets   []string
	// SupportedPaymentMethods is open-ended ('card', 'bank_transfer',
	// 'local_method:<name>', 'crypto_rail', ...) - never a fixed enum the
	// orchestrator hardcodes (docs/decisions/0022 §2).
	SupportedPaymentMethods []string
	// SupportedCountries is ISO country codes - an adapter-declared
	// PAYMENT-RAIL capability fact (e.g. "this local method only settles
	// in Brazil"), never a jurisdiction/regulatory value. Empty means "not
	// country-restricted" - never "all countries" by silent default; that
	// distinction is preserved verbatim through routing and capability
	// writes (docs/decisions/0022 §2).
	//
	// Stage 4I (docs/governance/stage-4i-payments-model.md) examined this
	// field against AssetAuthorization/Risk/Casino's jurisdiction-gating
	// absent-value contracts (reconnaissance C-3(d)) and concluded it is
	// NOT a specialization of them: it lives in ISO-3166 country-code
	// space, never `jurisdictions.code` space, and today - RouteProvider's
	// dimension 2 remains TODO(jurisdiction) below - it is consumed ONLY
	// by WriteCapability's narrow-only invariant, never by a live
	// routing or regulatory decision. Two rules bind whoever eventually
	// builds routing dimension 2 or any country->jurisdiction mapping:
	//  1. A SupportedCountries value is NEVER substituted for a
	//     jurisdictions.code value, or vice versa, without an explicit,
	//     stored, audited mapping reviewed by `security` (canonical model
	//     §4.5) - conflating the two code spaces is exactly the
	//     concept-conflation the platform's jurisdiction model forbids.
	//  2. This field's "empty = unrestricted" CAPABILITY default must
	//     NEVER be reused as the absent-value CONTRACT for a future
	//     jurisdiction/regulatory gate. That gate resolves the operation's
	//     jurisdiction via internal/jurisdiction.Resolve and independently
	//     satisfies the platform-wide invariant - "an absent jurisdiction
	//     must never let a jurisdiction-dependent policy be evaluated as
	//     if it did not exist" - rather than inheriting permissiveness
	//     from a provider's own capability declaration.
	SupportedCountries     []string
	SupportsDeposit        bool
	SupportsWithdrawal     bool
	SupportsRefundReversal bool
	AmountLimits           []AmountLimit
	SettlementBehavior     string
	CallbackCapabilities   CallbackCapability
}

// ProviderCapability is the full effective capability row: layer (a)
// (AdapterCapability) plus layer (b), the operator-configured tenant-scoped
// fields (docs/decisions/0022 §2). Mirrors the provider_capabilities /
// provider_capability_amount_limits tables (migration 0024). An effective
// capability for a given (tenant, brand, provider) is always exactly one
// such row - resolved by whole-row replacement, never a per-field merge
// (docs/decisions/0022 §3) - see LoadCapability/ListRoutingCandidates.
type ProviderCapability struct {
	AdapterCapability
	ID uuid.UUID
	// TenantID is always set. BrandID is nil for a tenant-wide row -
	// "NULL brand_id = available to every brand under that tenant"
	// (docs/decisions/0022 §2).
	TenantID uuid.UUID
	BrandID  *uuid.UUID
	// Priority is a tenant-configurable ranking among otherwise-equal
	// (post health-filter) candidates - distinct from the health-based
	// ranking in payment-orchestration.md §6. Lower value = preferred.
	Priority int
	Status   CapabilityStatus
}

// ProviderHealth is payment-orchestration.md §6's health/circuit-breaker
// state for one provider.
type ProviderHealth struct {
	ProviderID          string
	RollingSuccessRate  float64 // 0.0-1.0 over a rolling window
	RollingLatencyP99Ms int64
	CircuitState        CircuitState
	LastUpdated         time.Time
}

// DepositRequest is the canonical, adapter-agnostic shape for asking a
// PaymentProvider to initiate a deposit. Per docs/decisions/0022 §4.1,
// this shape has NO free-form passthrough field (no RawPayload/Extra/
// Metadata map) - every field crossing the PaymentProvider interface is
// explicitly enumerated and typed, so there is structurally no way for an
// adapter's caller to smuggle unexpected data through it.
type DepositRequest struct {
	// MerchantReference is the orchestrator's own DepositIntent id
	// (as a string), passed so a real adapter can echo it back to the
	// provider as an order/merchant reference for the provider's own
	// support/reconciliation tooling. It is NEVER used by the platform to
	// resolve tenant/player/wallet on the way back in - a callback is
	// always resolved via (provider_id, provider_reference) against the
	// platform's own deposit_intents row plus the tenant the verified
	// callback credential belongs to, never from any value carried in a
	// payload (payment-orchestration.md §10).
	MerchantReference string
	Amount            int64
	AssetCode         string
	PaymentMethod     string
}

// DepositResult is what a PaymentProvider adapter returns synchronously
// from Deposit. Outcome is never OutcomeSucceeded here - per
// financial-transaction-flows.md Flow 1, a deposit's success is only ever
// established via a verified callback or QueryStatus, never as the
// immediate return value of initiating one (even a rail that settles
// "instantly" still confirms via its own async callback so every adapter
// goes through the identical ReceiveCallback path - docs/decisions/0022
// §6's "not a special code path" rule, applied to the mock too).
type DepositResult struct {
	// Outcome is OutcomePending (awaiting an async callback/redirect
	// completion), OutcomeDeclined (a synchronous, definite decline - e.g.
	// hosted-field validation failed before any redirect), or
	// OutcomeAmbiguous (the initiation call itself timed out/errored
	// ambiguously).
	Outcome Outcome
	// ProviderReference is the provider's own reference for this attempt.
	// Required when Outcome is Pending or Ambiguous (needed to correlate
	// a later callback or to call QueryStatus); may be empty for a
	// Declined outcome if the provider never assigned one.
	ProviderReference string
	// RedirectURL/HostedFieldToken: at most one is set, depending on the
	// payment method's integration shape. Both empty for a Declined
	// outcome.
	RedirectURL      string
	HostedFieldToken string
	// DeclineReason and Cascadable are set only when Outcome ==
	// OutcomeDeclined. Cascadable is this adapter's own declared
	// taxonomy of "provider-specific, retriable elsewhere" (true) vs.
	// "player-specific, no cascade would help" (false) -
	// payment-orchestration.md §5's per-adapter OPEN DECISION, resolved
	// per adapter, not by the orchestrator.
	DeclineReason string
	Cascadable    bool
}

// WithdrawRequest/WithdrawResult mirror Deposit's shape for the send leg
// of a withdrawal. Not called by this stage's orchestrator (withdrawal
// orchestration is NOT IMPLEMENTED yet - see the package doc comment) but
// defined now so every PaymentProvider adapter, including the mock, has a
// complete, conformance-tested implementation ready for that stage.
type WithdrawRequest struct {
	MerchantReference string
	Amount            int64
	AssetCode         string
	PaymentMethod     string
}

type WithdrawResult struct {
	Outcome           Outcome
	ProviderReference string
	DeclineReason     string
	Cascadable        bool
}

// StatusResult is QueryStatus's return value - used both to resolve an
// ambiguous outcome (payment-orchestration.md §5) and, in principle, for
// reconciliation polling. AssetCode/Amount let a caller cross-check the
// provider's own confirmed facts against what was requested, rather than
// trusting the reference match alone.
type StatusResult struct {
	ProviderReference string
	Outcome           Outcome
	Amount            int64
	AssetCode         string
	// DeclineReason/Cascadable are set only when Outcome ==
	// OutcomeDeclined - same cascade-eligibility taxonomy as
	// DepositResult.Cascadable (payment-orchestration.md §5), surfaced
	// here too so an ambiguous outcome resolved via QueryStatus can still
	// cascade correctly rather than being forced to a conservative
	// non-cascadable default.
	DeclineReason string
	Cascadable    bool
}

// CallbackEventType distinguishes which financial-transaction-flows.md
// flow a parsed callback corresponds to. Only the two this stage
// implements are defined - withdrawal callback types are added when
// withdrawal orchestration is implemented.
type CallbackEventType string

const (
	CallbackEventDeposit         CallbackEventType = "deposit"
	CallbackEventDepositReversal CallbackEventType = "deposit_reversal"
)

// CallbackEvent is HandleCallback's canonical, parsed-and-verified output.
// Per docs/decisions/0022 §4.1, this shape has no free-form passthrough
// field either. Signature verification happens inside HandleCallback
// BEFORE any payload field is used to construct this value
// (payment-orchestration.md §10) - an adapter that cannot verify the
// payload returns an error, never a CallbackEvent.
type CallbackEvent struct {
	EventType CallbackEventType
	// ProviderReference is THIS event's own provider reference - for a
	// deposit_reversal this is the reversal's own reference, distinct
	// from the original deposit's (financial-transaction-flows.md Flow 2).
	ProviderReference string
	// OriginalProviderReference is set only for EventType ==
	// CallbackEventDepositReversal: the reference of the deposit being
	// reversed.
	OriginalProviderReference string
	Outcome                   Outcome
	Amount                    int64
	AssetCode                 string
	// DeclineReason/Cascadable are set only when Outcome ==
	// OutcomeDeclined - see StatusResult's identical fields.
	DeclineReason string
	Cascadable    bool
}

// PaymentProvider is the interface every adapter (fiat or crypto payment
// gateway, real or mock) implements identically - payment-orchestration.md
// §2. No PSP SDK type or vendor-specific shape ever appears in this
// interface's method signatures; provider-specific behavior lives
// entirely inside the adapter's own implementation, translating to/from
// these canonical shapes.
type PaymentProvider interface {
	Deposit(ctx context.Context, req DepositRequest) (DepositResult, error)
	Withdraw(ctx context.Context, req WithdrawRequest) (WithdrawResult, error)
	// QueryStatus looks up the current status of a previously-initiated
	// deposit or withdrawal by this adapter's own provider reference.
	QueryStatus(ctx context.Context, providerReference string) (StatusResult, error)
	// HandleCallback verifies an inbound provider callback's signature and
	// parses it into a canonical CallbackEvent. req carries the raw wire
	// body plus headers and the caller-verified (route-resolved) tenant/
	// provider id; cred is the single credential the Orchestrator already
	// resolved and equality-checked for (req.TenantID, req.ProviderID)
	// BEFORE calling this method (docs/decisions/0022 §3 amendment) - an
	// adapter never resolves its own credential and never tries more than
	// the one it is given. A real adapter's own signature scheme maps onto
	// this contract per the amendment's point 3 (per-merchant keys, or a
	// signed account id equal to cred's bound account); the mock's scheme
	// (mock.go) is documented there as the reference implementation. Any
	// failure BEFORE verification succeeds - including an unparseable body
	// - returns ErrCallbackSignatureInvalid, or ErrInboundKeyMaterial once
	// verification has succeeded and a key-material scan then rejects the
	// payload; never any other error for a pre-verification failure
	// (docs/decisions/0022 §3 amendment point 7, the adapter error
	// contract). A verified-but-structurally-malformed body returns a
	// DIFFERENT, adapter-specific sentinel (the mock's
	// ErrCallbackMalformedBody) that a caller maps to a 4xx, never to the
	// uniform pre-verification 401.
	HandleCallback(ctx context.Context, req InboundCallback, cred WebhookCredential) (CallbackEvent, error)
	// Capabilities returns this adapter's own declared, static layer only
	// - never tenant/brand/priority/status (docs/decisions/0022 §2).
	Capabilities() AdapterCapability
	HealthStatus(ctx context.Context) (ProviderHealth, error)
}
