// Package awssm is the awssm:// secret-store backend: provider credential
// secrets read from AWS Secrets Manager (ADR 0093 §6; the "Amendment"
// section; security review docs/plans/stage-10.3-planning/
// 07-w2a-design-review-security.md §4.1's recommendation to W3b).
//
// This is the ONLY package in the module permitted to import
// "github.com/aws/aws-sdk-go-v2/..." (ADR 0093 §6 point "confined to one
// package"; security review C12 condition 2). TestImportBoundary_
// NoOtherPackageImportsAWSSDK enforces that.
//
// Scope note (HD-10.3-2): this package contains NO IAM, KMS or `deploy/`
// change. It cannot run against real AWS until a task role exists; see the
// STAGING REQUIRED list in ADR 0093 §8 and the stage-10.3 W3b delivery
// report. cmd/platform-api constructs it only when SECRETSTORE_BACKENDS
// names awssm in an explicit staging/production environment.
//
// Credentials: ECS container (task-role) credentials ONLY, by allow-list
// (security gate W2/W3 finding S-1). New sets the container endpoint
// provider explicitly (AWS_CONTAINER_CREDENTIALS_RELATIVE_URI against the
// fixed ECS agent address, no proxy), loads with empty shared config/
// credentials file lists, and wraps the provider so a credential whose
// Source is not that provider is refused on every retrieval; the SDK's
// default chain is never consulted. On top of that, New refuses - in
// EVERY environment - any static-credential signal (AWS_ACCESS_KEY_ID,
// AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, AWS_PROFILE and the SDK
// aliases AWS_ACCESS_KEY, AWS_SECRET_KEY, AWS_DEFAULT_PROFILE, or a shared
// credentials/config file), any endpoint/CA/trust-root override
// (AWS_ENDPOINT_URL, every AWS_ENDPOINT_URL_<SERVICE>, AWS_CA_BUNDLE,
// SSL_CERT_FILE, SSL_CERT_DIR), and any credential-source redirection
// (AWS_CONTAINER_CREDENTIALS_FULL_URI, AWS_EC2_METADATA_SERVICE_ENDPOINT,
// AWS_WEB_IDENTITY_TOKEN_FILE, AWS_ROLE_ARN) (finding S-2). awssm:// refs
// must be full secret ARNs (secretstore.ParseRef), pinning the account.
// The fingerprint check remains the backstop against a redirected store.
// NOT decided here (open under HD-10.3-2): HTTPS_PROXY handling for the
// Secrets Manager client itself, and whether IRSA/web identity is ever an
// authorized source.
//
// Ambient SDK tuning is also refused and pinned (security gate W2/W3
// finding N-1): AWS_DEFAULTS_MODE=auto makes the SDK's config loader dial
// EC2 IMDS (169.254.169.254) during LoadDefaultConfig to classify the
// environment for timeouts, which is network I/O at init, forbidden by ADR
// 0093 §6. AWS_MAX_ATTEMPTS and AWS_RETRY_MODE would let the ambient
// environment change SDK retry behaviour underneath the secretstore
// circuit breaker's fixed StoreCallTimeout/BreakerTripThreshold budget. New
// refuses all three env vars outright, and also pins DefaultsModeStandard
// and a fixed retryer (standard mode, pinnedRetryMaxAttempts) explicitly in
// loadOpts as defence in depth, so even a future refusal regression cannot
// silently reintroduce ambient tuning.
//
// Reads always pin an exact Secrets Manager VersionId. GetSecretValueInput
// never carries VersionStage, so a stage label ("AWSCURRENT" etc.) can
// never be requested through this backend, matching the ADR 0093 ref CHECK
// (secretstore.ParseRef already refuses a ref whose versionId is not a
// 32-64 character token, or that names "versionStage" at all — this is
// belt-and-suspenders at the SDK-call boundary).
//
// SDK client logging is off (ClientLogMode zero value; never overridden).
// No request or response body — and never SecretString/SecretBinary — is
// logged or wrapped into an error. Every failure is mapped to the closed
// secretstore.ErrorClass taxonomy before it is returned, so a real store
// error string, SDK output or path never leaves this package.
package awssm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/endpointcreds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
)

// MaxSecretBytes is the largest accepted secret value, consistent with the
// devfile backend's MaxFileBytes.
const MaxSecretBytes = 64 << 10

// pinnedRetryMaxAttempts is the fixed SDK retry budget for the Secrets
// Manager client (security gate W2/W3 finding N-1). secretstore.Fetcher
// bounds one store call, INCLUDING its retries, to StoreCallTimeout (2s;
// internal/secretstore/fetcher.go), and opens the circuit breaker after
// BreakerTripThreshold (3) consecutive failures. A larger attempt count
// (the SDK standard-mode default is 3, and AWS_MAX_ATTEMPTS could set it far
// higher) risks a single call's backoff alone consuming the whole
// StoreCallTimeout budget, which would starve the breaker of the fast
// failures it needs to trip promptly. 2 attempts (one retry) leaves retry
// headroom without materially eating into the 2s budget at standard-mode
// backoff, and is small enough that ambient tuning cannot make one call's
// retries dominate the breaker's window.
const pinnedRetryMaxAttempts = 2

// client is the narrow slice of the Secrets Manager SDK client this
// package depends on. Production uses the real *secretsmanager.Client;
// tests use an in-package fake implementing this interface, so no test in
// this repository ever dials a real AWS endpoint.
type client interface {
	GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// Store is the awssm:// backend. It deliberately implements
// providerkind.ProductionEligible (via MarkProductionEligible) and NOT
// providerkind.Synthetic: unlike devfile and memstore, this is the real
// backend intended to eventually serve production traffic, once a human
// authorizes the IAM architecture (HD-10.3-2) and it is wired in.
type Store struct {
	c client
}

// MarkProductionEligible implements providerkind.ProductionEligible by
// structural typing (that package is not imported here, by design — see
// its own doc comment on why no marked type needs to import it).
func (s *Store) MarkProductionEligible() {}

// Scheme implements secretstore.Store.
func (s *Store) Scheme() string { return secretstore.SchemeAWSSecretsManager }

// New validates the environment and constructs a Store backed by a real
// AWS Secrets Manager client, using the default (task-role/container)
// credential chain only. region must be non-empty and comes explicitly
// from configuration (ADR 0093 §6: "the region comes explicitly from
// configuration") — never inferred from the ambient environment.
func New(ctx context.Context, cfg config.Config, region string) (*Store, error) {
	credEndpoint, err := preflight(cfg, region)
	if err != nil {
		return nil, err
	}

	// Credentials by ALLOW-LIST (security gate W2/W3 finding S-1): the ONLY
	// provider is the ECS container (task-role) endpoint, set explicitly,
	// so the SDK's default chain (env keys and their aliases, shared
	// files, SSO, process, web identity, IMDS) is never consulted. Its
	// HTTP client has no proxy: the endpoint is the link-local ECS agent,
	// and routing task-role credentials through a proxy is never correct.
	// containerOnlyProvider refuses any retrieved credential whose Source
	// is not that provider.
	var credHTTP endpointcreds.HTTPClient = awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) { tr.Proxy = nil })
	if httpClientOverride != nil {
		credHTTP = httpClientOverride
	}
	creds := aws.NewCredentialsCache(containerOnlyProvider{inner: endpointcreds.New(credEndpoint, func(o *endpointcreds.Options) {
		o.HTTPClient = credHTTP
	})})

	loadOpts := pinnedLoadOptions(region, creds)
	if httpClientOverride != nil {
		loadOpts = append(loadOpts, awsconfig.WithHTTPClient(httpClientOverride))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("awssm: load AWS config: %w", err)
	}
	// The SDK keeps a *CredentialsCache as given; anything else means the
	// allow-list did not take effect, so refuse rather than run with an
	// unknown source.
	if awsCfg.Credentials != aws.CredentialsProvider(creds) {
		return nil, errors.New("awssm: refusing to start: credential provider is not the authorized container provider")
	}

	sm := secretsmanager.NewFromConfig(awsCfg, func(o *secretsmanager.Options) {
		o.ClientLogMode = aws.ClientLogMode(0)
		// No BaseEndpoint is ever set here — the only way this client
		// could be redirected is the env vars refused above.
	})
	return newStore(sm), nil
}

// pinnedLoadOptions is every awsconfig.LoadOptions setting New applies,
// factored out so a test can call awsconfig.LoadDefaultConfig with exactly
// these options and inspect the resulting aws.Config directly (security
// gate W2/W3 finding N-1: proving the pin, not just the refusal, since
// defence in depth means these must hold even if the ambientTuningOverride/
// endpointOverride refusals above ever regressed).
func pinnedLoadOptions(region string, creds aws.CredentialsProvider) []func(*awsconfig.LoadOptions) error {
	return []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
		// SDK logging off (ADR 0093 §6; C12.6). The zero value already
		// means "no logging"; this is explicit so a later default change
		// upstream cannot silently turn logging on.
		awsconfig.WithClientLogMode(aws.ClientLogMode(0)),
		// No shared config/credentials file is ever read (S-1).
		awsconfig.WithSharedConfigFiles([]string{}),
		awsconfig.WithSharedCredentialsFiles([]string{}),
		awsconfig.WithCredentialsProvider(creds),
		// Defaults mode and retry behaviour are pinned explicitly (N-1), as
		// defence in depth on top of the ambientTuningOverrideSignal
		// refusal above: AWS_DEFAULTS_MODE=auto would otherwise make
		// LoadDefaultConfig dial EC2 IMDS during New (network at init,
		// forbidden by ADR 0093 §6), and AWS_MAX_ATTEMPTS/AWS_RETRY_MODE
		// would let the environment change retry behaviour underneath the
		// secretstore circuit breaker's fixed StoreCallTimeout budget. See
		// pinnedRetryMaxAttempts for why 2.
		awsconfig.WithDefaultsMode(aws.DefaultsModeStandard),
		awsconfig.WithRetryMode(aws.RetryModeStandard),
		awsconfig.WithRetryMaxAttempts(pinnedRetryMaxAttempts),
	}
}

// preflight is every refusal New applies before it touches the SDK: the
// environment allow-list, an explicit region, no static-credential signal,
// no endpoint/CA/trust-root override, no credential-source redirection,
// and a well-formed ECS task-role credential endpoint (returned). None of
// it makes a network call. NewWithSDKFake runs exactly the same checks.
func preflight(cfg config.Config, region string) (string, error) {
	if err := cfg.ValidateSecretBackendScheme(config.SecretBackendAWSSecretsManager); err != nil {
		return "", fmt.Errorf("awssm: %w", err)
	}
	if region == "" {
		return "", errors.New("awssm: region must be set explicitly")
	}
	if reason, found := staticCredentialSignal(); found {
		return "", fmt.Errorf("awssm: refusing to start: static AWS credential signal present (%s); "+
			"task-role credentials only, in every environment", reason)
	}
	if reason, found := endpointOverrideSignal(); found {
		return "", fmt.Errorf("awssm: refusing to start: endpoint/CA override present (%s)", reason)
	}
	if reason, found := credentialSourceOverrideSignal(); found {
		return "", fmt.Errorf("awssm: refusing to start: credential-source override present (%s); "+
			"only the ECS container task-role endpoint is authorized", reason)
	}
	if reason, found := ambientTuningOverrideSignal(); found {
		return "", fmt.Errorf("awssm: refusing to start: ambient SDK tuning override present (%s); "+
			"defaults mode and retry behaviour are pinned explicitly, never taken from the environment", reason)
	}
	return containerCredentialsEndpoint()
}

// ecsCredentialsHost is the fixed, link-local ECS agent address the
// container credential provider talks to (the SDK's own
// ecsContainerEndpoint). Only the RELATIVE URI comes from the environment.
const ecsCredentialsHost = "169.254.170.2"

// ecsRelativeURIPattern bounds AWS_CONTAINER_CREDENTIALS_RELATIVE_URI to a
// plain absolute path (the agent sets "/v2/credentials/<id>"): no scheme,
// host, userinfo, port, query or fragment can be smuggled in.
var ecsRelativeURIPattern = regexp.MustCompile(`^/[A-Za-z0-9._~-][A-Za-z0-9._~/-]{0,511}$`)

// containerCredentialsEndpoint returns the ECS task-role credential URL,
// or refuses: the container provider is the only authorized credential
// source (S-1), so without it awssm cannot run and startup is refused
// rather than falling back to anything else.
func containerCredentialsEndpoint() (string, error) {
	rel, _ := lookupEnv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
	if rel == "" {
		return "", errors.New("awssm: refusing to start: AWS_CONTAINER_CREDENTIALS_RELATIVE_URI is not set; " +
			"the ECS container (task-role) credential provider is the only authorized source")
	}
	if !ecsRelativeURIPattern.MatchString(rel) {
		return "", errors.New("awssm: refusing to start: AWS_CONTAINER_CREDENTIALS_RELATIVE_URI is not a plain path")
	}
	u, err := url.Parse("http://" + ecsCredentialsHost + rel)
	if err != nil || u.Host != ecsCredentialsHost || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("awssm: refusing to start: AWS_CONTAINER_CREDENTIALS_RELATIVE_URI does not resolve to the ECS agent")
	}
	return u.String(), nil
}

// errUnauthorizedCredentialSource is returned by containerOnlyProvider for
// a credential from any provider other than the container endpoint.
var errUnauthorizedCredentialSource = errors.New("awssm: credential from an unauthorized source")

// containerOnlyProvider wraps the credential provider and refuses every
// retrieved credential whose Source is not the container endpoint
// provider (S-1: "refuse unless the resolved Credentials.Source is that
// provider"). The check runs on every retrieval, not at New, because New
// makes no network call (ADR 0093 §6).
type containerOnlyProvider struct {
	inner aws.CredentialsProvider
}

func (p containerOnlyProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	c, err := p.inner.Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, err
	}
	if c.Source != endpointcreds.ProviderName {
		return aws.Credentials{}, errUnauthorizedCredentialSource
	}
	return c, nil
}

// httpClientOverride is nil in production. This package's tests set it
// (TestMain) to a tripwire client that refuses every request, and New then
// uses it for BOTH the credential endpoint and Secrets Manager, so no test
// in this package can ever reach the network through a Store built by New
// (code review #6).
var httpClientOverride interface {
	Do(*http.Request) (*http.Response, error)
}

// newStore builds a Store over any client (real or the test fake). Kept
// unexported: production code must go through New, which enforces the
// guards above.
func newStore(c client) *Store { return &Store{c: c} }

// staticCredentialViolationEnvVars are refused in EVERY environment
// (security review §4.1's recommendation to W3b, tightening ADR 0093 §6
// condition 4 which named only staging/production).
// The SDK also reads the aliases AWS_ACCESS_KEY, AWS_SECRET_KEY and
// AWS_DEFAULT_PROFILE (security gate W2/W3 finding S-1). With the
// allow-listed provider they are never used, but they are refused anyway so
// an operator sees the misconfiguration at startup.
var staticCredentialViolationEnvVars = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_PROFILE",
	"AWS_ACCESS_KEY",
	"AWS_SECRET_KEY",
	"AWS_DEFAULT_PROFILE",
}

// endpointOverrideEnvVars are refused in every environment (ADR 0093 §6
// condition 5; gate W2/W3 finding S-2): endpoint overrides, a custom AWS
// CA bundle, and Go's own system trust-root overrides (crypto/x509 honours
// SSL_CERT_FILE/SSL_CERT_DIR on Linux, which would re-open exactly the TLS
// interception AWS_CA_BUNDLE is refused for). Every other
// AWS_ENDPOINT_URL_<SERVICE> variable is refused by prefix
// (endpointOverrideSignal).
var endpointOverrideEnvVars = []string{
	"AWS_ENDPOINT_URL",
	"AWS_ENDPOINT_URL_SECRETS_MANAGER",
	"AWS_ENDPOINT_URL_STS",
	"AWS_CA_BUNDLE",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
}

// endpointOverridePrefix covers every service-specific endpoint override.
const endpointOverridePrefix = "AWS_ENDPOINT_URL_"

// credentialSourceOverrideEnvVars redirect where the SDK obtains
// credentials (S-2). The allow-listed container provider never reads them;
// they are refused so that nothing configured to hand the process another
// account's credentials is silently accepted. Whether IRSA/web identity
// is ever an authorized source is an open HD-10.3-2 decision; until it is
// made, it is refused.
var credentialSourceOverrideEnvVars = []string{
	"AWS_CONTAINER_CREDENTIALS_FULL_URI",
	"AWS_EC2_METADATA_SERVICE_ENDPOINT",
	"AWS_WEB_IDENTITY_TOKEN_FILE",
	"AWS_ROLE_ARN",
}

// ambientTuningOverrideEnvVars are refused in every environment (security
// gate W2/W3 finding N-1): AWS_DEFAULTS_MODE=auto makes LoadDefaultConfig
// dial EC2 IMDS during New (network at init, forbidden by ADR 0093 §6);
// AWS_MAX_ATTEMPTS and AWS_RETRY_MODE would let the environment change SDK
// retry behaviour underneath the secretstore circuit breaker's fixed
// budget. New pins DefaultsModeStandard and a fixed retryer explicitly
// regardless, so these are refused for the same reason the other
// environment overrides above are: an operator must see the
// misconfiguration at startup, not have it silently overridden.
var ambientTuningOverrideEnvVars = []string{
	"AWS_DEFAULTS_MODE",
	"AWS_MAX_ATTEMPTS",
	"AWS_RETRY_MODE",
}

// ambientTuningOverrideSignal reports the first ambient SDK tuning env var
// found, if any.
func ambientTuningOverrideSignal() (string, bool) {
	for _, k := range ambientTuningOverrideEnvVars {
		if v, ok := lookupEnv(k); ok && v != "" {
			return k, true
		}
	}
	return "", false
}

// lookupEnv/statPath are indirections so tests can simulate a shared
// credentials file or a HOME directory without touching the real
// filesystem or environment of the test process outside t.Setenv.
var (
	lookupEnv = os.LookupEnv
	environ   = os.Environ
	statPath  = os.Stat
	userHome  = os.UserHomeDir
)

// staticCredentialSignal reports the first static-credential signal found,
// if any: an explicit env var, or a shared credentials/config file that
// exists on disk (named by env var or at its default path).
func staticCredentialSignal() (string, bool) {
	for _, k := range staticCredentialViolationEnvVars {
		if v, ok := lookupEnv(k); ok && v != "" {
			return k, true
		}
	}
	if v, ok := lookupEnv("AWS_SHARED_CREDENTIALS_FILE"); ok && v != "" {
		if _, err := statPath(v); err == nil {
			return "AWS_SHARED_CREDENTIALS_FILE", true
		}
	}
	if v, ok := lookupEnv("AWS_CONFIG_FILE"); ok && v != "" {
		if _, err := statPath(v); err == nil {
			return "AWS_CONFIG_FILE", true
		}
	}
	if home, err := userHome(); err == nil && home != "" {
		if _, err := statPath(filepath.Join(home, ".aws", "credentials")); err == nil {
			return "~/.aws/credentials", true
		}
		if _, err := statPath(filepath.Join(home, ".aws", "config")); err == nil {
			return "~/.aws/config", true
		}
	}
	return "", false
}

// endpointOverrideSignal reports the first endpoint/CA/trust-root override
// env var found, if any, including any AWS_ENDPOINT_URL_<SERVICE>.
func endpointOverrideSignal() (string, bool) {
	for _, k := range endpointOverrideEnvVars {
		if v, ok := lookupEnv(k); ok && v != "" {
			return k, true
		}
	}
	for _, kv := range environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, endpointOverridePrefix) && v != "" {
			return k, true
		}
	}
	return "", false
}

// credentialSourceOverrideSignal reports the first credential-source
// redirection env var found, if any.
func credentialSourceOverrideSignal() (string, bool) {
	for _, k := range credentialSourceOverrideEnvVars {
		if v, ok := lookupEnv(k); ok && v != "" {
			return k, true
		}
	}
	return "", false
}

// reservedVersionStages are Secrets Manager's built-in stage labels. A ref
// pinning one of these as its "version" is refused defensively — belt and
// suspenders, since secretstore.ParseRef's regex (32-64 char version
// token) already makes this unreachable through the only public
// constructor of secretstore.Ref.
var reservedVersionStages = map[string]bool{
	"AWSCURRENT":  true,
	"AWSPENDING":  true,
	"AWSPREVIOUS": true,
}

// validateVersion refuses an empty version or a reserved stage label,
// standing in front of every GetSecretValue call. It is unreachable in
// practice through the only public constructor of secretstore.Ref
// (ParseRef's awssm pattern requires a 32-64 character token), so it is
// tested directly rather than through Store.Get.
func validateVersion(version string) error {
	if version == "" || reservedVersionStages[version] {
		return secretstore.NewError(secretstore.ClassInvalidVersion)
	}
	return nil
}

// Get implements secretstore.Store. It calls GetSecretValue with a pinned
// VersionId ONLY — VersionStage is never set on the request.
func (s *Store) Get(ctx context.Context, ref secretstore.Ref) (secretstore.Secret, error) {
	if err := ctx.Err(); err != nil {
		return secretstore.Secret{}, secretstore.NewError(secretstore.ClassUnavailable)
	}
	if ref.Scheme() != secretstore.SchemeAWSSecretsManager {
		return secretstore.Secret{}, secretstore.NewError(secretstore.ClassInvalidRef)
	}
	version := ref.Version()
	if err := validateVersion(version); err != nil {
		return secretstore.Secret{}, err
	}
	secretID := ref.Path()
	if secretID == "" {
		return secretstore.Secret{}, secretstore.NewError(secretstore.ClassInvalidRef)
	}

	out, err := s.c.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId:  aws.String(secretID),
		VersionId: aws.String(version),
		// VersionStage is deliberately never set.
	})
	if err != nil {
		if ctx.Err() != nil {
			return secretstore.Secret{}, secretstore.NewError(secretstore.ClassUnavailable)
		}
		return secretstore.Secret{}, classifyError(err)
	}
	if ctx.Err() != nil {
		return secretstore.Secret{}, secretstore.NewError(secretstore.ClassUnavailable)
	}

	b, err := secretBytes(out)
	if err != nil {
		return secretstore.Secret{}, err
	}
	if fragment := ref.Fragment(); fragment != "" {
		b, err = extractJSONField(b, fragment)
		if err != nil {
			return secretstore.Secret{}, err
		}
	}
	if len(b) < 1 || len(b) > MaxSecretBytes {
		zero(b)
		return secretstore.Secret{}, secretstore.NewError(secretstore.ClassStoreConfig)
	}
	secret := secretstore.NewSecret(b)
	zero(b)
	return secret, nil
}

// secretBytes extracts SecretBinary or SecretString from out. Exactly one
// of them is expected to be set (Secrets Manager guarantees this); if
// neither is, that is a store_config failure — never a panic and never a
// zero-length secret silently accepted.
func secretBytes(out *secretsmanager.GetSecretValueOutput) ([]byte, error) {
	switch {
	case out == nil:
		return nil, secretstore.NewError(secretstore.ClassStoreConfig)
	case out.SecretBinary != nil:
		b := make([]byte, len(out.SecretBinary))
		copy(b, out.SecretBinary)
		return b, nil
	case out.SecretString != nil:
		return []byte(*out.SecretString), nil
	default:
		return nil, secretstore.NewError(secretstore.ClassStoreConfig)
	}
}

// extractJSONField parses b as a JSON object and returns the string value
// at key, as raw bytes (not the JSON-quoted form). A malformed JSON body,
// a missing key or a non-string value at that key is store_config — never
// a panic, and the raw JSON is never included in the error.
func extractJSONField(b []byte, key string) ([]byte, error) {
	defer zero(b)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, secretstore.NewError(secretstore.ClassStoreConfig)
	}
	raw, ok := m[key]
	for k := range m {
		if k != key {
			zero([]byte(m[k]))
		}
	}
	if !ok {
		return nil, secretstore.NewError(secretstore.ClassStoreConfig)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		zero([]byte(raw))
		return nil, secretstore.NewError(secretstore.ClassStoreConfig)
	}
	out := []byte(s)
	zero([]byte(raw))
	return out, nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// classifyError maps an SDK/smithy error to the closed secretstore error
// taxonomy (security review §5 "failures that count"). No SDK error text,
// request id or wrapped detail is ever attached to the returned error.
func classifyError(err error) error {
	var rnf *smtypes.ResourceNotFoundException
	if errors.As(err, &rnf) {
		return secretstore.NewError(secretstore.ClassNotFound)
	}
	var invParam *smtypes.InvalidParameterException
	if errors.As(err, &invParam) {
		return secretstore.NewError(secretstore.ClassInvalidVersion)
	}
	var invReq *smtypes.InvalidRequestException
	if errors.As(err, &invReq) {
		return secretstore.NewError(secretstore.ClassStoreConfig)
	}
	var decryptFail *smtypes.DecryptionFailure
	if errors.As(err, &decryptFail) {
		// A KMS decrypt denial is an authorization failure, not a
		// transient one: it does not count toward the breaker.
		return secretstore.NewError(secretstore.ClassAccessDenied)
	}
	var internalErr *smtypes.InternalServiceError
	if errors.As(err, &internalErr) {
		return secretstore.NewError(secretstore.ClassUnavailable)
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDeniedException", "UnrecognizedClientException", "AccessDenied":
			return secretstore.NewError(secretstore.ClassAccessDenied)
		case "ThrottlingException", "TooManyRequestsException", "RequestLimitExceeded":
			return secretstore.NewError(secretstore.ClassUnavailable)
		}
		if apiErr.ErrorFault() == smithy.FaultServer {
			return secretstore.NewError(secretstore.ClassUnavailable)
		}
		// An unrecognized client-fault API error is treated as
		// store_config: it is a per-request problem, not a store
		// availability problem, so it must not trip the breaker.
		return secretstore.NewError(secretstore.ClassStoreConfig)
	}

	// Anything else (network error, timeout, DNS failure, a panic-free
	// unknown condition): fail closed as unavailable, the same default
	// secretstore.ClassOf itself applies to a non-*Error value.
	return secretstore.NewError(secretstore.ClassUnavailable)
}
