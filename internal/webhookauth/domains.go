package webhookauth

// Per-domain parameters of the platform-defined MOCK wire scheme (ADR 0022
// §3 point 8; design 01-webhook-trust-design.md §A). Three independent
// separations per domain: signing-input prefix, header names, mock key
// label. The payments values are byte-identical to Stage 10.1's (they are
// what internal/payments' SigningInputPrefix/HeaderSignature/HeaderKeyID
// constants and mock key derivation have always used) and must never
// change: a change would silently invalidate every in-flight mock callback
// and break the payments regression guarantee.
const (
	PaymentsSigningPrefix   = "igaming.payments.webhook.v1"
	PaymentsSignatureHeader = "X-Payments-Signature"
	PaymentsKeyIDHeader     = "X-Payments-Key-Id"
	PaymentsMockKeyLabel    = "igaming/payments-mock-webhook/v1"

	KYCSigningPrefix   = "igaming.kyc.webhook.v1"
	KYCSignatureHeader = "X-KYC-Signature"
	KYCKeyIDHeader     = "X-KYC-Key-Id"
	KYCMockKeyLabel    = "igaming/kyc-mock-webhook/v1"

	CasinoSigningPrefix   = "igaming.casino.webhook.v1"
	CasinoSignatureHeader = "X-Casino-Signature"
	CasinoKeyIDHeader     = "X-Casino-Key-Id"
	CasinoMockKeyLabel    = "igaming/casino-mock-webhook/v1"
)

// PaymentsScheme is the payments domain's MOCK wire scheme.
func PaymentsScheme() Scheme {
	return Scheme{Prefix: PaymentsSigningPrefix, SignatureHeader: PaymentsSignatureHeader, KeyIDHeader: PaymentsKeyIDHeader}
}

// KYCScheme is the KYC domain's MOCK wire scheme.
func KYCScheme() Scheme {
	return Scheme{Prefix: KYCSigningPrefix, SignatureHeader: KYCSignatureHeader, KeyIDHeader: KYCKeyIDHeader}
}

// CasinoScheme is the casino domain's MOCK wire scheme.
func CasinoScheme() Scheme {
	return Scheme{Prefix: CasinoSigningPrefix, SignatureHeader: CasinoSignatureHeader, KeyIDHeader: CasinoKeyIDHeader}
}
