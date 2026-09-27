package httpserver

import (
	"time"

	"github.com/Diansalas/igaming-platform/internal/admission"
)

// webhookDomain names one of the three provider-facing webhook domains
// (ADR 0097 §3/§4). Always fixed by the calling route, never derived from
// request input.
type webhookDomain string

const (
	domainPayments webhookDomain = "payments"
	domainCasino   webhookDomain = "casino"
	domainKYC      webhookDomain = "kyc"

	// unknownComponent is the collapsed value for a tenantKey or
	// providerKey that isn't in the corresponding bounded, server-owned
	// set (ADR 0097 §4.2).
	unknownComponent = "_unknown"
)

// WebhookRateBurst is one GCRA (rate, burst) pair, in httpserver's own
// plain-value vocabulary (ADR 0097 §20 AC1: "internal/config produces
// plain values; httpserver maps them to admission types" - this type,
// not internal/config's, is what internal/admission-backed code in this
// package actually consumes; cmd/platform-api/main.go maps
// config.WebhookAdmissionConfig's identical shape into this one field by
// field, so internal/httpserver never imports internal/config).
type WebhookRateBurst struct {
	Rate  float64
	Burst int
}

// WebhookAdmissionOverride mirrors config.WebhookAdmissionOverride (ADR
// 0097 §9.2) - see that type's doc comment for the full contract.
type WebhookAdmissionOverride struct {
	Domain     string
	ProviderID string
	Tenant     string
	Rate       float64
	Burst      int
}

// WebhookAdmissionSettings is the fully-resolved ADR 0097 configuration
// httpserver.New consumes to build its admission runtime. The zero value
// (Enabled: false) disables the admission layer entirely and every
// webhook route behaves exactly as it did before PRH-I4 - this is what
// keeps every pre-existing httpserver test, which constructs a Deps
// literal without setting this field, passing unchanged.
type WebhookAdmissionSettings struct {
	Enabled bool

	PerIPRPS   float64
	PerIPBurst int

	PreAuthRate        map[string]WebhookRateBurst
	PreAuthUnknownRate map[string]WebhookRateBurst
	VerifiedRate       map[string]WebhookRateBurst

	InFlightGlobal  int
	InFlightPerKey  int
	InFlightUnknown int

	DBGateGlobal  int
	DBGatePerKey  int
	DBGateUnknown int
	DBGateWait    time.Duration

	DomainTxPerTenant int
	DomainWait        time.Duration

	BodyReadTimeout time.Duration

	DirectoryRefresh time.Duration
	DirectoryCap     int

	IdleEvict       time.Duration
	VerifiedMaxKeys int
	PerIPMaxKeys    int

	Overrides []WebhookAdmissionOverride

	// Clock defaults to admission.RealClock() when nil - tests inject a
	// FakeClock here to drive T5/T8/T9/etc. deterministically.
	Clock admission.Clock
}
