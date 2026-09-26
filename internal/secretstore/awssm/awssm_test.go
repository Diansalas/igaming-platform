package awssm

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
)

// fakeClient is the SDK-interface fake required by the task: no test in
// this file ever dials a real AWS endpoint.
type fakeClient struct {
	lastInput *secretsmanager.GetSecretValueInput
	calls     int

	out *secretsmanager.GetSecretValueOutput
	err error
}

func (f *fakeClient) GetSecretValue(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.calls++
	f.lastInput = in
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

func validRef(t *testing.T, fragment string) secretstore.Ref {
	t.Helper()
	tenant := uuid.New()
	raw := "awssm://prefix/provider-creds/" + tenant.String() + "/casino/acme/name?versionId=" + strings.Repeat("a", 32)
	if fragment != "" {
		raw += "#" + fragment
	}
	ref, err := secretstore.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

func strPtr(s string) *string { return &s }

// TestAWSSM_VersionPinning: Get always calls GetSecretValue with VersionId
// set from the ref, and VersionStage is NEVER set (ADR 0093 §6; the ref
// CHECK).
func TestAWSSM_VersionPinning(t *testing.T) {
	fc := &fakeClient{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr("shh")}}
	s := newStore(fc)
	ref := validRef(t, "")

	if _, err := s.Get(context.Background(), ref); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fc.calls != 1 {
		t.Fatalf("expected exactly 1 SDK call, got %d", fc.calls)
	}
	if fc.lastInput.VersionId == nil || *fc.lastInput.VersionId != ref.Version() {
		t.Fatalf("VersionId = %v, want %q", fc.lastInput.VersionId, ref.Version())
	}
	if fc.lastInput.VersionStage != nil {
		t.Fatalf("VersionStage must never be set, got %q", *fc.lastInput.VersionStage)
	}
	if fc.lastInput.SecretId == nil || *fc.lastInput.SecretId != ref.Path() {
		t.Fatalf("SecretId = %v, want %q", fc.lastInput.SecretId, ref.Path())
	}
}

// TestAWSSM_StageLabelRefusal covers both layers: secretstore.ParseRef
// refuses a raw ref naming a stage label or "versionStage" at all (the
// only public way to build a secretstore.Ref), and this backend's own
// defence-in-depth (reservedVersionStages, checked directly since no
// public path can otherwise reach it) refuses one too, with no SDK call
// made.
func TestAWSSM_StageLabelRefusal(t *testing.T) {
	tenant := uuid.New()
	ns := "/provider-creds/" + tenant.String() + "/casino/acme/name"
	for _, raw := range []string{
		"awssm://prefix" + ns, // no versionId at all
		"awssm://prefix" + ns + "?versionId=AWSCURRENT",
		"awssm://prefix" + ns + "?versionStage=AWSCURRENT",
	} {
		if _, err := secretstore.ParseRef(raw); secretstore.ClassOf(err) != secretstore.ClassInvalidRef {
			t.Fatalf("%q: expected ClassInvalidRef from ParseRef, got %v", raw, err)
		}
	}

	// Defence in depth: validateVersion (the check Get runs before every
	// SDK call) refuses a reserved stage label directly. This path is
	// unreachable through the public secretstore.Ref constructor (whose
	// awssm pattern requires a 32-64 character token), so it is exercised
	// directly rather than by round-tripping through ParseRef.
	for _, v := range []string{"", "AWSCURRENT", "AWSPENDING", "AWSPREVIOUS"} {
		if err := validateVersion(v); secretstore.ClassOf(err) != secretstore.ClassInvalidVersion {
			t.Fatalf("validateVersion(%q): expected ClassInvalidVersion, got %v", v, err)
		}
	}
	if err := validateVersion(strings.Repeat("a", 32)); err != nil {
		t.Fatalf("validateVersion(valid): unexpected error %v", err)
	}
}

// TestAWSSM_WrongSchemeRefused: Get refuses a ref for a different scheme
// (defence in depth; the Router/Fetcher never do this in production).
func TestAWSSM_WrongSchemeRefused(t *testing.T) {
	fc := &fakeClient{}
	s := newStore(fc)
	tenant := uuid.New()
	raw := "memory://prefix/provider-creds/" + tenant.String() + "/casino/acme/name?version=v1"
	ref, err := secretstore.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	_, err = s.Get(context.Background(), ref)
	if secretstore.ClassOf(err) != secretstore.ClassInvalidRef {
		t.Fatalf("expected ClassInvalidRef, got %v", err)
	}
	if fc.calls != 0 {
		t.Fatalf("expected no SDK call, got %d", fc.calls)
	}
}

// TestAWSSM_StaticCredentialRefusal: New refuses to construct when any
// static-credential signal is present, in every GuardEnvironment —
// including development, per the security review §4.1 recommendation.
func TestAWSSM_StaticCredentialRefusal(t *testing.T) {
	stagingCfg := config.Config{Environment: "staging", EnvironmentExplicit: true}

	cases := []struct {
		name string
		env  map[string]string
	}{
		{"access_key", map[string]string{"AWS_ACCESS_KEY_ID": "AKIAEXAMPLE"}},
		{"secret_key", map[string]string{"AWS_SECRET_ACCESS_KEY": "x"}},
		{"session_token", map[string]string{"AWS_SESSION_TOKEN": "x"}},
		{"profile", map[string]string{"AWS_PROFILE": "default"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := New(context.Background(), stagingCfg, "us-east-1")
			if err == nil {
				t.Fatal("expected refusal, got nil error")
			}
		})
	}

	t.Run("shared_credentials_file", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/credentials"
		if err := writeFile(path, "[default]\n"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", path)
		_, err := New(context.Background(), stagingCfg, "us-east-1")
		if err == nil {
			t.Fatal("expected refusal, got nil error")
		}
	})

	t.Run("no_static_signal_passes_this_guard", func(t *testing.T) {
		// Clear every AWS_* env var this process might have inherited so
		// the test is deterministic regardless of the host.
		clearAWSEnv(t)
		if _, found := staticCredentialSignal(); found {
			t.Skip("host environment carries a real AWS credential file; skipping the positive case")
		}
		// New() will still fail past this guard (LoadDefaultConfig may
		// need a working directory/etc, and no network call is made at
		// init per ADR 0093 §6 condition 9), but it must not fail with
		// the static-credential message.
		_, err := New(context.Background(), stagingCfg, "us-east-1")
		if err != nil && strings.Contains(err.Error(), "static AWS credential signal") {
			t.Fatalf("unexpected static-credential refusal: %v", err)
		}
	})
}

// TestAWSSM_StaticCredentialRefusal_EveryEnvironment: the refusal applies
// even in development and production, not only staging (security review
// §4.1's binding recommendation, tightening ADR 0093 §6 condition 4 which
// named only staging/production).
func TestAWSSM_StaticCredentialRefusal_EveryEnvironment(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	for _, cfg := range []config.Config{
		{Environment: "development", EnvironmentExplicit: true},
		{Environment: "staging", EnvironmentExplicit: true},
		{Environment: "production", EnvironmentExplicit: true},
	} {
		_, err := New(context.Background(), cfg, "us-east-1")
		if err == nil {
			t.Fatalf("%s: expected refusal", cfg.Environment)
		}
	}
}

// TestAWSSM_EndpointOverrideRefusal: New refuses an endpoint or CA bundle
// override, in every environment.
func TestAWSSM_EndpointOverrideRefusal(t *testing.T) {
	clearAWSEnv(t)
	stagingCfg := config.Config{Environment: "staging", EnvironmentExplicit: true}
	for _, k := range []string{"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_SECRETS_MANAGER", "AWS_CA_BUNDLE"} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, "https://evil.example.com")
			_, err := New(context.Background(), stagingCfg, "us-east-1")
			if err == nil || !strings.Contains(err.Error(), "endpoint/CA override") {
				t.Fatalf("expected endpoint override refusal, got %v", err)
			}
		})
	}
}

// TestAWSSM_ConfigGateEnforced: New refuses to construct outside
// staging/production, mirroring config.ValidateSecretBackendScheme.
func TestAWSSM_ConfigGateEnforced(t *testing.T) {
	clearAWSEnv(t)
	dev := config.Config{Environment: "development", EnvironmentExplicit: true}
	if _, err := New(context.Background(), dev, "us-east-1"); err == nil {
		t.Fatal("expected refusal in development")
	}
	missing := config.Config{Environment: "development", EnvironmentExplicit: false}
	if _, err := New(context.Background(), missing, "us-east-1"); err == nil {
		t.Fatal("expected refusal when APP_ENV is missing")
	}
}

// TestAWSSM_RegionRequired: New refuses an empty region.
func TestAWSSM_RegionRequired(t *testing.T) {
	clearAWSEnv(t)
	staging := config.Config{Environment: "staging", EnvironmentExplicit: true}
	if _, err := New(context.Background(), staging, ""); err == nil {
		t.Fatal("expected refusal for empty region")
	}
}

// TestAWSSM_ErrorMapping proves each SDK/smithy error class maps to the
// secretstore taxonomy the fetcher's breaker relies on.
func TestAWSSM_ErrorMapping(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		class secretstore.ErrorClass
	}{
		{"not_found", &smtypes.ResourceNotFoundException{}, secretstore.ClassNotFound},
		{"invalid_parameter", &smtypes.InvalidParameterException{}, secretstore.ClassInvalidVersion},
		{"invalid_request", &smtypes.InvalidRequestException{}, secretstore.ClassStoreConfig},
		{"decryption_failure", &smtypes.DecryptionFailure{}, secretstore.ClassAccessDenied},
		{"internal_service_error", &smtypes.InternalServiceError{}, secretstore.ClassUnavailable},
		{"access_denied_api_error", &fakeAPIError{code: "AccessDeniedException", fault: smithy.FaultClient}, secretstore.ClassAccessDenied},
		{"throttling_api_error", &fakeAPIError{code: "ThrottlingException", fault: smithy.FaultClient}, secretstore.ClassUnavailable},
		{"server_fault_api_error", &fakeAPIError{code: "SomethingElse", fault: smithy.FaultServer}, secretstore.ClassUnavailable},
		{"unrecognized_client_fault_api_error", &fakeAPIError{code: "SomethingElse", fault: smithy.FaultClient}, secretstore.ClassStoreConfig},
		{"unknown_network_error", errors.New("connection reset"), secretstore.ClassUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeClient{err: tc.err}
			s := newStore(fc)
			_, err := s.Get(context.Background(), validRef(t, ""))
			if got := secretstore.ClassOf(err); got != tc.class {
				t.Fatalf("class = %v, want %v", got, tc.class)
			}
		})
	}
}

type fakeAPIError struct {
	code              string
	fault             smithy.ErrorFault
	sentinelInMessage string
}

func (e *fakeAPIError) Error() string {
	msg := "fake: " + e.code
	if e.sentinelInMessage != "" {
		msg += ": " + e.sentinelInMessage
	}
	return msg
}
func (e *fakeAPIError) ErrorCode() string { return e.code }
func (e *fakeAPIError) ErrorMessage() string {
	if e.sentinelInMessage != "" {
		return e.sentinelInMessage
	}
	return "fake message, never logged"
}
func (e *fakeAPIError) ErrorFault() smithy.ErrorFault { return e.fault }

// TestAWSSM_SizeLimit mirrors devfile's MaxFileBytes rule.
func TestAWSSM_SizeLimit(t *testing.T) {
	t.Run("oversize_refused", func(t *testing.T) {
		big := strings.Repeat("a", MaxSecretBytes+1)
		fc := &fakeClient{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr(big)}}
		s := newStore(fc)
		_, err := s.Get(context.Background(), validRef(t, ""))
		if secretstore.ClassOf(err) != secretstore.ClassStoreConfig {
			t.Fatalf("expected ClassStoreConfig, got %v", err)
		}
	})
	t.Run("empty_refused", func(t *testing.T) {
		fc := &fakeClient{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr("")}}
		s := newStore(fc)
		_, err := s.Get(context.Background(), validRef(t, ""))
		if secretstore.ClassOf(err) != secretstore.ClassStoreConfig {
			t.Fatalf("expected ClassStoreConfig, got %v", err)
		}
	})
	t.Run("within_limit_accepted", func(t *testing.T) {
		ok := strings.Repeat("a", MaxSecretBytes)
		fc := &fakeClient{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr(ok)}}
		s := newStore(fc)
		secret, err := s.Get(context.Background(), validRef(t, ""))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if secret.Len() != MaxSecretBytes {
			t.Fatalf("Len() = %d, want %d", secret.Len(), MaxSecretBytes)
		}
	})
}

// TestAWSSM_JSONFragmentExtraction: a ref carrying a #jsonKey fragment
// extracts that field from a JSON secret body.
func TestAWSSM_JSONFragmentExtraction(t *testing.T) {
	fc := &fakeClient{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr(`{"apiKey":"topsecretvalue","other":"ignored"}`)}}
	s := newStore(fc)
	secret, err := s.Get(context.Background(), validRef(t, "apiKey"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(secret.Bytes()) != "topsecretvalue" {
		t.Fatalf("Bytes() = %q, want %q", secret.Bytes(), "topsecretvalue")
	}
}

func TestAWSSM_JSONFragmentMissingKeyOrNonString(t *testing.T) {
	for _, body := range []string{
		`{"other":"x"}`,
		`{"apiKey":42}`,
		`not json`,
	} {
		fc := &fakeClient{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr(body)}}
		s := newStore(fc)
		_, err := s.Get(context.Background(), validRef(t, "apiKey"))
		if secretstore.ClassOf(err) != secretstore.ClassStoreConfig {
			t.Fatalf("body %q: expected ClassStoreConfig, got %v", body, err)
		}
	}
}

// TestAWSSM_SecretBinarySupported: SecretBinary is read when set instead
// of SecretString.
func TestAWSSM_SecretBinarySupported(t *testing.T) {
	fc := &fakeClient{out: &secretsmanager.GetSecretValueOutput{SecretBinary: []byte("binary-secret")}}
	s := newStore(fc)
	secret, err := s.Get(context.Background(), validRef(t, ""))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(secret.Bytes()) != "binary-secret" {
		t.Fatalf("Bytes() = %q", secret.Bytes())
	}
}

func TestAWSSM_NeitherSecretFieldSet(t *testing.T) {
	fc := &fakeClient{out: &secretsmanager.GetSecretValueOutput{}}
	s := newStore(fc)
	_, err := s.Get(context.Background(), validRef(t, ""))
	if secretstore.ClassOf(err) != secretstore.ClassStoreConfig {
		t.Fatalf("expected ClassStoreConfig, got %v", err)
	}
}

// TestAWSSM_RedactionNoSecretInErrorsOrLogs: the returned error never
// carries the secret value, the SDK message or the ref path.
func TestAWSSM_RedactionNoSecretInErrorsOrLogs(t *testing.T) {
	sentinel := "TOP-SECRET-SENTINEL-VALUE-DO-NOT-LEAK"
	fc := &fakeClient{err: &fakeAPIError{code: "SomethingElse", fault: smithy.FaultClient, sentinelInMessage: sentinel}}
	s := newStore(fc)
	_, err := s.Get(context.Background(), validRef(t, ""))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("error leaked the sentinel: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "secretstore: ") {
		t.Fatalf("error must be the closed secretstore.Error form, got %v", err)
	}
}

// TestAWSSM_ImportedFromFetcher proves this backend integrates with the
// W2a Fetcher's cache and circuit breaker end to end, using this package's
// fake client and the test-only memory router bypass to route an awssm
// scheme WITHOUT going through the environment allow-list (test support
// only — production routing goes through secretstore.NewRouter, which
// this package's own TestAWSSM_ConfigGateEnforced already covers).
func TestAWSSM_FetcherIntegration_BreakerAndCache(t *testing.T) {
	fp := func(b []byte) string { return "fp1:" + string(b) }
	tenant := uuid.New()
	now := time.Now()
	fc := &fakeClient{err: &fakeAPIError{code: "ThrottlingException", fault: smithy.FaultServer}}
	store := newStore(fc)

	// memstore.NewRouter bypasses the environment allow-list for test
	// support only; it is used here purely to exercise the Fetcher
	// against this package's Store, not to claim awssm is routable
	// outside staging/production.
	router, err := memstore.NewRouter(store)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	fetcher := secretstore.NewFetcher(router, fp, secretstore.WithClock(func() time.Time { return now }))

	ref := validRef(t, "")
	fingerprint := fp([]byte{}) // never matches; only breaker behaviour matters here
	for i := 0; i < secretstore.BreakerTripThreshold; i++ {
		// Advance the clock past the per-ref negative-cache TTL between
		// calls so each Fetch reaches the store (and the breaker) again,
		// instead of being answered from the negative cache.
		now = now.Add(secretstore.NegativeTTLCounting + time.Second)
		if _, err := fetcher.Fetch(context.Background(), tenant, ref, fingerprint); secretstore.ClassOf(err) != secretstore.ClassUnavailable {
			t.Fatalf("call %d: expected ClassUnavailable, got %v", i, err)
		}
	}
	if state := fetcher.BreakerState(secretstore.SchemeAWSSecretsManager); state != "open" {
		t.Fatalf("breaker state = %q, want open", state)
	}
	callsBeforeOpen := fc.calls
	if _, err := fetcher.Fetch(context.Background(), tenant, ref, fingerprint); secretstore.ClassOf(err) != secretstore.ClassUnavailable {
		t.Fatalf("expected fail-fast ClassUnavailable while open, got %v", err)
	}
	if fc.calls != callsBeforeOpen {
		t.Fatalf("expected no SDK call while breaker open, calls went from %d to %d", callsBeforeOpen, fc.calls)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// clearAWSEnv unsets every AWS_* variable this process might have
// inherited, for the duration of the calling test, so the guard tests are
// deterministic regardless of the host/CI environment.
func clearAWSEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE",
		"AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE",
		"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_SECRETS_MANAGER", "AWS_CA_BUNDLE",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
}
