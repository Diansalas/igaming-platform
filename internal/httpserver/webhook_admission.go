package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/admission"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/txscope"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// webhookAdmissionRuntime is the constructed ADR 0097 admission state for
// one server instance: the GCRA limiters (A3 known/unknown, B1), the
// bulkheads (A4a in-flight, A4b DB gate, B2 domain-tx), the tenant
// directory and the log suppressor. Built once by newWebhookAdmission,
// stored on Deps (unexported field), and shared by every webhook route -
// never reconstructed per request.
//
// A nil *webhookAdmissionRuntime always admits (used when
// WebhookAdmissionSettings.Enabled is false) - every call in this file is
// nil-receiver-safe, so callers never need a separate "is admission even
// on" branch.
type webhookAdmissionRuntime struct {
	settings WebhookAdmissionSettings
	clock    admission.Clock
	logger   *slog.Logger

	perIP *admission.GCRALimiter // A2, shared across domains; nil if disabled

	preAuthKnown   map[webhookDomain]*admission.GCRALimiter
	preAuthUnknown map[webhookDomain]*admission.GCRALimiter
	verified       map[webhookDomain]*admission.GCRALimiter

	inflight *admission.Bulkhead // A4a, shared across domains (key includes domain)
	dbGate   dbGateAcquirer      // A4b, shared across domains (key includes domain)
	domainTx *admission.Bulkhead // B2, keyed by tenant_id only (shared across domains)

	directory *webhookTenantDirectory

	suppress *admission.Suppressor

	// overflowLogged latches, per limiter tier key, whether the ADR 0097
	// §7 "the configured bound is misestimated -> _overflow shared bucket
	// + error" log line has already been emitted - so a sustained
	// overflow logs exactly once, not once per request (security review
	// Low: "emit the overflow error log", previously
	// admission.GCRALimiter.OverflowOccurred() was computed but never
	// actually checked/logged anywhere).
	overflowLogged sync.Map // map[string]struct{}
}

// logOverflowOnce emits webhook_admission_limiter_overflow exactly once
// per tier key, the first time lim reports it has ever folded a key into
// its shared overflow bucket.
func (rt *webhookAdmissionRuntime) logOverflowOnce(tierKey string, lim *admission.GCRALimiter) {
	if lim == nil || !lim.OverflowOccurred() {
		return
	}
	if _, already := rt.overflowLogged.LoadOrStore(tierKey, struct{}{}); already {
		return
	}
	rt.logger.Error("webhook_admission_limiter_overflow", "tier", tierKey)
}

// newWebhookAdmission constructs the runtime from settings. pool feeds the
// tenant directory (ADR 0097 §4.2); it may be nil only in tests that never
// call refreshDirectory/gate anything through it.
func newWebhookAdmission(settings WebhookAdmissionSettings, pool *db.Pool, logger *slog.Logger) *webhookAdmissionRuntime {
	if !settings.Enabled {
		return nil
	}
	clock := settings.Clock
	if clock == nil {
		clock = admission.RealClock()
	}
	if logger == nil {
		logger = slog.Default()
	}

	rt := &webhookAdmissionRuntime{
		settings:       settings,
		clock:          clock,
		logger:         logger,
		preAuthKnown:   map[webhookDomain]*admission.GCRALimiter{},
		preAuthUnknown: map[webhookDomain]*admission.GCRALimiter{},
		verified:       map[webhookDomain]*admission.GCRALimiter{},
		inflight:       admission.NewBulkhead(settings.InFlightGlobal),
		dbGate:         admission.NewBulkhead(settings.DBGateGlobal),
		domainTx:       admission.NewBulkhead(orElse(settings.DomainTxPerTenant*10000, settings.DomainTxPerTenant*10000)),
		suppress:       admission.NewSuppressor(10*time.Second, clock),
	}
	// B2's global cap has no separate config knob (ADR 0097 §9.1 only
	// specifies the PER-TENANT cap) - it is bounded only by the database
	// pool itself via each domain transaction's own connection acquire,
	// so the bulkhead's global cap is set generously high (effectively
	// "per-tenant cap is the only real limit here"); a huge but finite
	// value keeps the same bounded-map data structure and Release
	// discipline as A4a/A4b rather than a special-cased unlimited path.
	rt.domainTx = admission.NewBulkhead(1 << 20)

	if settings.PerIPRPS > 0 {
		rt.perIP = admission.NewGCRALimiter(settings.PerIPRPS, settings.PerIPBurst, settings.PerIPMaxKeys, settings.IdleEvict, unknownComponent, clock)
	}
	for _, d := range []webhookDomain{domainPayments, domainCasino, domainKYC} {
		known := settings.PreAuthRate[string(d)]
		unknown := settings.PreAuthUnknownRate[string(d)]
		verified := settings.VerifiedRate[string(d)]
		// maxKeys for pre-auth known/unknown limiters: the unknown
		// limiter only ever tracks ONE key per domain (ADR 0097 §4.2 "one
		// unknown bucket per domain"), so a tiny cap is correct; the
		// known limiter is bounded by the same directory cap that bounds
		// tenantKey cardinality times the (small) provider registry.
		rt.preAuthKnown[d] = admission.NewGCRALimiter(known.Rate, known.Burst, settings.DirectoryCap*8+8, settings.IdleEvict, "_overflow", clock)
		rt.preAuthUnknown[d] = admission.NewGCRALimiter(unknown.Rate, unknown.Burst, 1, settings.IdleEvict, unknownComponent, clock)
		rt.verified[d] = admission.NewGCRALimiter(verified.Rate, verified.Burst, settings.VerifiedMaxKeys, settings.IdleEvict, "_overflow", clock)
	}
	if pool != nil {
		rt.directory = newWebhookTenantDirectory(identityPoolAdapter{db: pool}, settings.DirectoryCap, logger)
	}
	return rt
}

func orElse(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}

// Directory exposes the tenant directory for /readyz gating and the
// background refresher (main.go). Nil-safe.
func (rt *webhookAdmissionRuntime) Directory() *webhookTenantDirectory {
	if rt == nil {
		return nil
	}
	return rt.directory
}

// RunDirectoryRefresh starts the background refresher, blocking until ctx
// is cancelled (devops condition 2: caller runs this in its own goroutine,
// wired to the same shutdown context as http.Server.Shutdown). No-op if
// admission is disabled or has no directory.
func (rt *webhookAdmissionRuntime) RunDirectoryRefresh(ctx context.Context) {
	if rt == nil || rt.directory == nil {
		return
	}
	rt.directory.Run(ctx, rt.settings.DirectoryRefresh)
}

// LoadDirectory performs the synchronous initial load (ADR 0097 §7:
// "startup waits for the first load"). Returns the error for the caller
// to log; the directory stays not-Loaded() on failure, which gates
// /readyz and every webhook route (they answer 503 rather than run with
// unknown keying) until a later background refresh succeeds.
func (rt *webhookAdmissionRuntime) LoadDirectory(ctx context.Context) error {
	if rt == nil || rt.directory == nil {
		return nil
	}
	return rt.directory.Load(ctx)
}

// rateBurstFor resolves the effective (rate, burst) for a tier/domain/
// provider/tenant, applying any matching §9.2 override before falling
// back to def.
func (rt *webhookAdmissionRuntime) rateBurstFor(def WebhookRateBurst, domain webhookDomain, providerKey, tenantKey string) WebhookRateBurst {
	for _, o := range rt.settings.Overrides {
		if o.Domain != string(domain) || o.ProviderID != providerKey {
			continue
		}
		if o.Tenant != "" && o.Tenant != tenantKey {
			continue
		}
		return WebhookRateBurst{Rate: o.Rate, Burst: o.Burst}
	}
	return def
}

// errDBGateUnavailable is returned by gatedGetTenantBySlug/gatedReader
// when the A4b gate could not be acquired within its bounded wait (or
// would require reentrant acquisition, AC2(b)) - the caller maps this to
// a 503, never the uniform 401 (it is a capacity rejection, not an auth
// failure).
// Security review C1 of PRH-I4 (HIGH): this MUST be exactly
// webhookauth.ErrTenantReaderUnavailable, not a locally-defined sentinel,
// because gatedReader.WithTenantReadOnly's error crosses package
// boundaries (into internal/providercred's real Resolver, then back up
// through internal/webhookauth's AuthError construction, then through
// payments/casino/kyc's VerifyCallback) before this package's handlers
// ever see it again - errors.Is only survives that whole path if every
// layer checks for, and propagates, the SAME sentinel value.
var errDBGateUnavailable = webhookauth.ErrTenantReaderUnavailable

// dbGateAcquirer is the A4b gate's own interface, satisfied by
// *admission.Bulkhead in production. Security review round 4 (C1(a)/T4's
// N1): tests need a seam that can admit a KEY's first k acquisitions and
// then refuse, regardless of whether the earlier ones have already been
// released - a real Bulkhead cannot do that (it is a concurrent-holder
// cap, not a call counter), so this interface lets a test substitute a
// call-counting fake in place of the real Bulkhead for exactly this
// purpose, without weakening or reimplementing the real gate's own
// concurrency semantics anywhere production code runs.
type dbGateAcquirer interface {
	Acquire(key string, perKeyCap int, clock admission.Clock, wait time.Duration) (release func(), ok bool)
}

// gatedReaderMarkKey marks a context as already holding an A4b slot
// acquired by THIS gatedReader chain, so a nested call (architect review
// AC2(b): "gatedReader is non-reentrant... fails closed instead of
// acquiring again, since nested acquire at per-key cap 2 is a
// hold-and-wait self-deadlock") is refused rather than blocking forever.
type gatedReaderMarkKey struct{}

// gatedReader wraps *db.Pool so every WithTenantReadOnly call it makes is
// gated by the A4b bulkhead (ADR 0097 §5.3): acquired before the pool
// acquire, released strictly after WithTenantReadOnly returns - never
// across the secret-store Fetcher (webhookauth.ResolveAndSeal never calls
// WithTenantReadOnly around the Fetcher; INV-POOL is unaffected). It
// implements webhookauth.TenantReader, so it is passed directly in place
// of deps.DB to VerifyCallback.
type gatedReader struct {
	db         *db.Pool
	gate       dbGateAcquirer
	key        string
	perKeyCap  int
	clock      admission.Clock
	wait       time.Duration
	onRejected func()
}

func (g gatedReader) WithTenantReadOnly(ctx context.Context, tenantID uuid.UUID, fn db.TxFunc) error {
	if txscope.Held(ctx) {
		// A pooled transaction is already open on this call chain -
		// acquiring the gate here would be a hold-and-wait self-deadlock
		// risk (AC2(b)); fail closed instead.
		return errDBGateUnavailable
	}
	if held, _ := ctx.Value(gatedReaderMarkKey{}).(bool); held {
		return errDBGateUnavailable
	}
	release, ok := g.gate.Acquire(g.key, g.perKeyCap, g.clock, g.wait)
	if !ok {
		if g.onRejected != nil {
			g.onRejected()
		}
		return errDBGateUnavailable
	}
	defer release()
	markedCtx := context.WithValue(ctx, gatedReaderMarkKey{}, true)
	return g.db.WithTenantReadOnly(markedCtx, tenantID, fn)
}

// gatedGetTenantBySlug wraps identity.GetTenantBySlug in the same A4b
// gate gatedReader uses (ADR 0097 §5.3: "webhookPreamble wraps its
// GetTenantBySlug call in gate.Acquire(tenantKey)").
func gatedGetTenantBySlug(ctx context.Context, gate dbGateAcquirer, key string, perKeyCap int, clock admission.Clock, wait time.Duration, pool *db.Pool, slug string) (identity.Tenant, error) {
	release, ok := gate.Acquire(key, perKeyCap, clock, wait)
	if !ok {
		return identity.Tenant{}, errDBGateUnavailable
	}
	defer release()
	return identity.GetTenantBySlug(ctx, pool, slug)
}

// preAuthKeys derives ADR 0097 §4.2's bounded (tenantKey, providerKey)
// pair. schemeRegistered reports whether providerID is a registered
// adapter in this domain (the process-global registry check ADR 0097
// requires - no DB, no tenant input).
func (rt *webhookAdmissionRuntime) preAuthKeys(tenantSlug, providerID string, schemeRegistered func(string) bool) (tenantKey, providerKey string) {
	tenantKey = unknownComponent
	if len(tenantSlug) <= 128 && rt.directory != nil && rt.directory.Contains(tenantSlug) {
		tenantKey = tenantSlug
	}
	providerKey = unknownComponent
	if len(providerID) > 0 && len(providerID) <= 128 && schemeRegistered != nil && schemeRegistered(providerID) {
		providerKey = providerID
	}
	if tenantKey == unknownComponent || providerKey == unknownComponent {
		return unknownComponent, unknownComponent
	}
	return tenantKey, providerKey
}

// writeAdmissionRejection writes ADR 0097 §6.1's generic, allow-listed
// rejection body: request id only, never signature/domain-specific
// content, so it can never be a signature or enumeration oracle (T15).
func writeAdmissionRejection(w http.ResponseWriter, requestID string, code apierror.Code, retryAfter time.Duration) {
	secs := int(retryAfter / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	msg := "too many requests; retry later"
	if code == apierror.CodeUnavailable {
		msg = "service temporarily unavailable; retry later"
	}
	apierror.Write(w, requestID, code, msg)
}

// admissionLogEvent is ADR 0097 §8's single allow-listed log event.
const admissionLogEvent = "webhook_admission_rejected"

func (rt *webhookAdmissionRuntime) logRejected(ctx context.Context, tier string, domain webhookDomain, tenantKey, providerKey string, tenantID *uuid.UUID, status int, retryAfter time.Duration, clientIP string) {
	suppressKey := tier + "|" + string(domain) + "|" + tenantKey + "|" + providerKey
	emit, suppressed := rt.suppress.ShouldLog(suppressKey)
	if !emit {
		return
	}
	requestID := observability.RequestIDFromContext(ctx)
	fields := []any{
		"request_id", requestID, "domain", string(domain), "tier", tier,
		"tenant_key", tenantKey, "provider_key", providerKey,
		"status", status, "retry_after_s", int(retryAfter / time.Second), "client_ip", clientIP,
		"suppressed", suppressed,
	}
	if tenantID != nil {
		fields = append(fields, "tenant_id", tenantID.String())
	}
	rt.logger.Warn(admissionLogEvent, fields...)
}

// markWebhookRouteForLogging is RL-F4's redaction hook (ADR 0097 §8/§17
// devops condition 3): every webhook request - including one rejected
// BEFORE admitPreAuth ever runs (e.g. the "webhooks not enabled on this
// deployment" 503 short-circuit every handler checks first, which is
// still reachable with arbitrary attacker-chosen path segments) - must
// have its access-log/panic-recovery path redacted to the matched route
// pattern. Security review Low ("two RL-F4 leftovers"): this was
// previously set only inside admitPreAuth, so a request that returned
// before admitPreAuth was ever called leaked the raw path. Every one of
// the three webhook handlers now calls this as its OWN first statement,
// before even the orchestrator-nil check; admitPreAuth also calls it
// (harmless, idempotent) for any caller that reaches it directly (tests).
func markWebhookRouteForLogging(r *http.Request) {
	if rs := observability.RequestStateFromContext(r.Context()); rs != nil {
		rs.LogPath = r.Pattern
	}
}

// admitPreAuth runs A2 (if enabled), A3 and A4a (ADR 0097 §3). It is the
// FIRST thing a webhook handler calls - strictly before any body read or
// DB work (ORD-1/ORD-2). On admission it returns a release func for the
// A4a slot (the caller must release it exactly once, typically via
// defer) and ok=true. On rejection it has already written the complete
// HTTP response and ok is false; the caller must return immediately.
//
// A nil receiver always admits (admission disabled) with a no-op release.
//
// retries429 reports the registered adapter's declared retry semantics for
// providerID (nil, or declared=false for an unknown/unregistered
// provider, defaults to 429 - security review C4 of PRH-I4: the ADR's own
// §6 text originally scoped adapter-declared status to B1 only; a
// provider that is REGISTERED is a static fact known even before
// verification succeeds - since providerKey is only ever the raw
// providerID when it names a registered adapter (preAuthKeys collapses
// anything else to "_unknown"), A3 can and must honor the same
// declaration B1 does, so a no-429-retry adapter never gets a 429 at
// EITHER tier).
func (rt *webhookAdmissionRuntime) admitPreAuth(w http.ResponseWriter, r *http.Request, domain webhookDomain, trustedProxyCount int, schemeRegistered func(string) bool, retries429 func(providerID string) (allows, declared bool)) (release func(), ok bool) {
	markWebhookRouteForLogging(r)

	noop := func() {}
	if rt == nil {
		return noop, true
	}

	// ADR 0097 §6.1/§7/T9: "limiter panic or internal error: 503 (the
	// admission layer recovers itself; it is never a 500 from
	// recoverMiddleware)". This is a SEPARATE, inner recover from the
	// generic outer recoverMiddleware (middleware.go), specific to
	// admission's own bounded, fail-closed contract.
	defer func() {
		if rec := recover(); rec != nil {
			requestID := observability.RequestIDFromContext(r.Context())
			rt.logger.Error("webhook_admission_panic_recovered", "panic", rec, "domain", string(domain))
			writeAdmissionRejection(w, requestID, apierror.CodeUnavailable, time.Second)
			release, ok = nil, false
		}
	}()

	ctx := r.Context()
	requestID := observability.RequestIDFromContext(ctx)
	clientIP := trustedProxyClientIP(r, trustedProxyCount)

	// ADR 0097 §7/T9 (security review Low, "make code match the comment"):
	// "Directory never loaded... webhook routes return 503 if called
	// anyway" - an EXPLICIT fail-closed gate, not merely letting every
	// tenant fall through to the non-authoritative "_unknown" bucket
	// (which could otherwise still admit and process a genuinely
	// well-formed, correctly-signed callback for a real tenant while the
	// admission subsystem itself is not yet ready).
	if rt.directory != nil && !rt.directory.Loaded() {
		rt.logRejected(ctx, "directory_unloaded", domain, "", "", nil, http.StatusServiceUnavailable, time.Second, clientIP)
		writeAdmissionRejection(w, requestID, apierror.CodeUnavailable, time.Second)
		return nil, false
	}

	// A2: per-source-IP tier, off by default (§4.4).
	if rt.perIP != nil {
		if admitted, retryAfter := rt.perIP.Allow(clientIP); !admitted {
			rt.logOverflowOnce("ip", rt.perIP)
			rt.logRejected(ctx, "ip", domain, "", "", nil, http.StatusTooManyRequests, retryAfter, clientIP)
			writeAdmissionRejection(w, requestID, apierror.CodeRateLimited, retryAfter)
			return nil, false
		}
	}

	tenantSlug := r.PathValue("tenantSlug")
	providerID := r.PathValue("providerID")
	tenantKey, providerKey := rt.preAuthKeys(tenantSlug, providerID, schemeRegistered)

	// A3: pre-auth bucket (domain, tenantKey, providerKey).
	limiter := rt.preAuthKnown[domain]
	tierKey := "preauth_known_" + string(domain)
	def := rt.settings.PreAuthRate[string(domain)]
	if tenantKey == unknownComponent {
		limiter = rt.preAuthUnknown[domain]
		tierKey = "preauth_unknown_" + string(domain)
		def = rt.settings.PreAuthUnknownRate[string(domain)]
	}
	rb := rt.rateBurstFor(def, domain, providerKey, tenantKey)
	preKey := string(domain) + "|" + tenantKey + "|" + providerKey
	admitted, retryAfter := limiter.AllowWithParams(preKey, rb.Rate, rb.Burst)
	rt.logOverflowOnce(tierKey, limiter)
	if !admitted {
		status := http.StatusTooManyRequests
		code := apierror.CodeRateLimited
		if retries429 != nil {
			if allows, declared := retries429(providerKey); declared && !allows {
				// Security review C4 of PRH-I4: a registered adapter that
				// does not retry 429 must never receive one, even at the
				// pre-auth tier (§6.3, ADR text amended).
				status = http.StatusServiceUnavailable
				code = apierror.CodeUnavailable
				retryAfter = time.Second
			}
		}
		rt.logRejected(ctx, "preauth", domain, tenantKey, providerKey, nil, status, retryAfter, clientIP)
		writeAdmissionRejection(w, requestID, code, retryAfter)
		return nil, false
	}

	// A4a: webhook in-flight bulkhead, non-blocking.
	inflightCap := rt.settings.InFlightPerKey
	if tenantKey == unknownComponent {
		inflightCap = rt.settings.InFlightUnknown
	}
	rel, admitted := rt.inflight.TryAcquire(preKey, inflightCap)
	if !admitted {
		rt.logRejected(ctx, "inflight", domain, tenantKey, providerKey, nil, http.StatusServiceUnavailable, time.Second, clientIP)
		writeAdmissionRejection(w, requestID, apierror.CodeUnavailable, time.Second)
		return nil, false
	}
	return rel, true
}

// newGatedReader builds the A4b gatedReader for one request, keyed
// exactly like the A3 preKey (ADR 0097 §5.3). Returns nil (meaning "use
// deps.DB directly, ungated") when admission is disabled.
func (rt *webhookAdmissionRuntime) newGatedReader(pool *db.Pool, domain webhookDomain, tenantKey, providerKey string) *gatedReader {
	if rt == nil {
		return nil
	}
	cap := rt.settings.DBGatePerKey
	if tenantKey == unknownComponent {
		cap = rt.settings.DBGateUnknown
	}
	return &gatedReader{
		db: pool, gate: rt.dbGate, clock: rt.clock, wait: rt.settings.DBGateWait,
		key: string(domain) + "|" + tenantKey + "|" + providerKey, perKeyCap: cap,
	}
}

// gatedTenantLookup runs GetTenantBySlug gated by A4b, or ungated when
// admission is disabled.
func (rt *webhookAdmissionRuntime) gatedTenantLookup(ctx context.Context, pool *db.Pool, domain webhookDomain, tenantKey, providerKey, slug string) (identity.Tenant, error) {
	if rt == nil {
		return identity.GetTenantBySlug(ctx, pool, slug)
	}
	cap := rt.settings.DBGatePerKey
	if tenantKey == unknownComponent {
		cap = rt.settings.DBGateUnknown
	}
	key := string(domain) + "|" + tenantKey + "|" + providerKey
	return gatedGetTenantBySlug(ctx, rt.dbGate, key, cap, rt.clock, rt.settings.DBGateWait, pool, slug)
}

// writeDBGateUnavailable answers ADR 0097's A4b capacity rejection
// (security review C2 of PRH-I4): 503 + Retry-After + a "db_gate"
// allow-listed log line, whether the rejection was observed as the bare
// errDBGateUnavailable sentinel (the platform-wide tenant lookup, or a
// domain's first pre-verification read that propagates it unwrapped) or
// wrapped inside the domain's own AuthError type (security review C1: a
// rejection from INSIDE credential resolution). tenantID is nil before
// the tenant is resolved (the slug-lookup call site). Nil-receiver-safe.
func (rt *webhookAdmissionRuntime) writeDBGateUnavailable(w http.ResponseWriter, r *http.Request, domain webhookDomain, tenantID *uuid.UUID, providerID string) {
	requestID := observability.RequestIDFromContext(r.Context())
	if rt != nil {
		rt.logRejected(r.Context(), "db_gate", domain, "", providerID, tenantID, http.StatusServiceUnavailable, time.Second, "")
	}
	writeAdmissionRejection(w, requestID, apierror.CodeUnavailable, time.Second)
}

// writeAdmissionUnavailableAuthError is writeDBGateUnavailable for a
// call site that already has a resolved tenantID (every handler's own
// AuthError branch, and their VerifyCallback-level errDBGateUnavailable
// check).
func (rt *webhookAdmissionRuntime) writeAdmissionUnavailableAuthError(w http.ResponseWriter, r *http.Request, domain webhookDomain, tenantID uuid.UUID, providerID string) {
	rt.writeDBGateUnavailable(w, r, domain, &tenantID, providerID)
}

// admitVerified runs B1 (verified rate bucket) then B2 (per-tenant domain
// transaction bulkhead), strictly after VerifyCallback succeeded and
// strictly before deps.DB.WithTenant opens the domain transaction (ORD-3).
// It is the ONLY place these two checks run, and it keys them off
// tenantID/providerID taken from the caller's own VerifiedCallback-derived
// values, never off preKey or any URL value (ORD-4, T17).
//
// On admission it returns a release func for the B2 slot that the caller
// MUST hold across not just deps.DB.WithTenant but any follow-up
// rejection-record transaction (ledger-finance C1), releasing it exactly
// once when the entire request is done. On rejection it has written the
// full HTTP response (429 for B1, using the adapter's declared retry
// semantics to choose 429 vs 503 per §6.3; 503 for B2) and ok is false.
func (rt *webhookAdmissionRuntime) admitVerified(w http.ResponseWriter, r *http.Request, domain webhookDomain, tenantID uuid.UUID, providerID string, retries429 func(providerID string) (allows, declared bool)) (release func(), ok bool) {
	noop := func() {}
	if rt == nil {
		return noop, true
	}

	// ADR 0097 §6.1/§7/T9: a panic inside B1/B2 is also recovered here as
	// a 503, never a 500 (see admitPreAuth's identical comment).
	defer func() {
		if rec := recover(); rec != nil {
			requestID := observability.RequestIDFromContext(r.Context())
			rt.logger.Error("webhook_admission_panic_recovered", "panic", rec, "domain", string(domain))
			writeAdmissionRejection(w, requestID, apierror.CodeUnavailable, time.Second)
			release, ok = nil, false
		}
	}()

	ctx := r.Context()
	requestID := observability.RequestIDFromContext(ctx)

	key := tenantID.String() + "|" + providerID
	def := rt.settings.VerifiedRate[string(domain)]
	rb := rt.rateBurstFor(def, domain, providerID, tenantID.String())
	admitted, retryAfter := rt.verified[domain].AllowWithParams(key, rb.Rate, rb.Burst)
	rt.logOverflowOnce("verified_"+string(domain), rt.verified[domain])
	if !admitted {
		status := http.StatusTooManyRequests
		code := apierror.CodeRateLimited
		if retries429 != nil {
			if allows, has := retries429(providerID); has && !allows {
				// §6.3: the adapter does not retry 429 - answer 503 instead,
				// which it MUST retry (a non-retrying-either adapter fails
				// registration, RequireRetrySemantics).
				status = http.StatusServiceUnavailable
				code = apierror.CodeUnavailable
				retryAfter = time.Second
			}
		}
		rt.logRejected(ctx, "verified", domain, "", providerID, &tenantID, status, retryAfter, "")
		writeAdmissionRejection(w, requestID, code, retryAfter)
		return nil, false
	}

	rel, admitted := rt.domainTx.Acquire(tenantID.String(), rt.settings.DomainTxPerTenant, rt.clock, rt.settings.DomainWait)
	if !admitted {
		rt.logRejected(ctx, "domain_bulkhead", domain, "", providerID, &tenantID, http.StatusServiceUnavailable, rt.settings.DomainWait, "")
		writeAdmissionRejection(w, requestID, apierror.CodeUnavailable, 2*time.Second)
		return nil, false
	}
	return rel, true
}
