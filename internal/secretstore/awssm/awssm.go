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
// report. It is wired into no binary by this change — cmd/platform-api is
// untouched.
//
// Credentials: task-role / default container credential chain ONLY. New
// refuses to construct if any static-credential signal is present
// (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN,
// AWS_PROFILE, a shared credentials/config file) — in EVERY environment,
// per the security review §4.1 recommendation that W3b apply the
// staging/production static-credential refusal (ADR 0093 §6 condition 4)
// everywhere, so `awssm` cannot be used with a personal AWS profile
// locally either. Endpoint overrides
// (AWS_ENDPOINT_URL/AWS_ENDPOINT_URL_SECRETS_MANAGER) and a custom CA
// bundle (AWS_CA_BUNDLE) are refused the same way (ADR 0093 §6 condition
// 5); the fingerprint check remains the backstop against a redirected
// store even if this refusal were ever bypassed.
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
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
)

// MaxSecretBytes is the largest accepted secret value, consistent with the
// devfile backend's MaxFileBytes.
const MaxSecretBytes = 64 << 10

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
	if err := cfg.ValidateSecretBackendScheme(config.SecretBackendAWSSecretsManager); err != nil {
		return nil, fmt.Errorf("awssm: %w", err)
	}
	if region == "" {
		return nil, errors.New("awssm: region must be set explicitly")
	}
	if reason, found := staticCredentialSignal(); found {
		return nil, fmt.Errorf("awssm: refusing to start: static AWS credential signal present (%s); "+
			"task-role credentials only, in every environment", reason)
	}
	if reason, found := endpointOverrideSignal(); found {
		return nil, fmt.Errorf("awssm: refusing to start: endpoint/CA override present (%s)", reason)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		// SDK logging off (ADR 0093 §6; C12.6). The zero value already
		// means "no logging"; this is explicit so a later default change
		// upstream cannot silently turn logging on.
		awsconfig.WithClientLogMode(aws.ClientLogMode(0)),
	)
	if err != nil {
		return nil, fmt.Errorf("awssm: load AWS config: %w", err)
	}

	sm := secretsmanager.NewFromConfig(awsCfg, func(o *secretsmanager.Options) {
		o.ClientLogMode = aws.ClientLogMode(0)
		// No BaseEndpoint is ever set here — the only way this client
		// could be redirected is the env vars refused above.
	})
	return newStore(sm), nil
}

// newStore builds a Store over any client (real or the test fake). Kept
// unexported: production code must go through New, which enforces the
// guards above.
func newStore(c client) *Store { return &Store{c: c} }

// staticCredentialViolationEnvVars are refused in EVERY environment
// (security review §4.1's recommendation to W3b, tightening ADR 0093 §6
// condition 4 which named only staging/production).
var staticCredentialViolationEnvVars = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_PROFILE",
}

// endpointOverrideEnvVars are refused in every environment (ADR 0093 §6
// condition 5).
var endpointOverrideEnvVars = []string{
	"AWS_ENDPOINT_URL",
	"AWS_ENDPOINT_URL_SECRETS_MANAGER",
	"AWS_CA_BUNDLE",
}

// lookupEnv/statPath are indirections so tests can simulate a shared
// credentials file or a HOME directory without touching the real
// filesystem or environment of the test process outside t.Setenv.
var (
	lookupEnv = os.LookupEnv
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

// endpointOverrideSignal reports the first endpoint/CA override env var
// found, if any.
func endpointOverrideSignal() (string, bool) {
	for _, k := range endpointOverrideEnvVars {
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
