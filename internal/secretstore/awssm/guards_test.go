package awssm

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/endpointcreds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
)

// tripwire is the HTTP client every Store built by New in this package's
// tests uses (installed by TestMain), so no test here can reach the
// network (code review #6). It records every request. A request to the
// ECS agent address gets an in-process, synthetic credential document (so
// the SDK proceeds to the Secrets Manager request, proving that client is
// overridden too); every other request is refused.
type tripwire struct {
	mu    sync.Mutex
	hosts []string
}

var errTripwire = errors.New("tripwire: a test attempted a network request")

func (w *tripwire) Do(r *http.Request) (*http.Response, error) {
	w.mu.Lock()
	w.hosts = append(w.hosts, r.URL.Host)
	w.mu.Unlock()
	if r.URL.Host == ecsCredentialsHost {
		body := fmt.Sprintf(`{"AccessKeyId":"AKIDSYNTHETICTEST","SecretAccessKey":"synthetic-not-a-secret","Token":"synthetic","Expiration":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)), Request: r,
		}, nil
	}
	return nil, errTripwire
}

func (w *tripwire) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.hosts...)
}

var testTripwire = &tripwire{}

func TestMain(m *testing.M) {
	httpClientOverride = testTripwire
	os.Exit(m.Run())
}

const testRelativeURI = "/v2/credentials/00000000-test-task"

func permittedEnv(t *testing.T) config.Config {
	t.Helper()
	clearAWSEnv(t)
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", testRelativeURI)
	return config.Config{Environment: "staging", EnvironmentExplicit: true}
}

// TestAWSSM_New_SuccessPathMakesNoNetworkCall (code review #6): with an
// environment that permits it (explicit staging, the container-credential
// endpoint, no static/override signal) New constructs a Store without any
// network request. A later Get goes only through the tripwire: first to
// the fixed ECS agent for (synthetic, in-process) credentials, which the
// container-only source check accepts, then to the region's Secrets
// Manager host, which the tripwire refuses - proving the override reaches
// both clients and that no real endpoint is ever contacted.
func TestAWSSM_New_SuccessPathMakesNoNetworkCall(t *testing.T) {
	cfg := permittedEnv(t)
	before := len(testTripwire.snapshot())
	s, err := New(context.Background(), cfg, "eu-west-1")
	if err != nil {
		t.Fatalf("New in a permitted environment: %v", err)
	}
	if got := testTripwire.snapshot(); len(got) != before {
		t.Fatalf("New made a network request: %v", got[before:])
	}
	if s.Scheme() != secretstore.SchemeAWSSecretsManager {
		t.Fatalf("scheme = %q", s.Scheme())
	}

	// The SDK retries a refused credential fetch with backoff; the bound
	// here only keeps the test short (in production the Fetcher's
	// StoreCallTimeout bounds the whole call).
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = s.Get(ctx, validRef(t, ""))
	if secretstore.ClassOf(err) != secretstore.ClassUnavailable {
		t.Fatalf("a Get whose every request is refused must be unavailable, got %v", err)
	}
	after := testTripwire.snapshot()[before:]
	if len(after) < 2 || after[0] != ecsCredentialsHost {
		t.Fatalf("the first request must be the credential fetch to %s through the tripwire, got %v", ecsCredentialsHost, after)
	}
	sawSM := false
	for _, h := range after[1:] {
		switch h {
		case "secretsmanager.eu-west-1.amazonaws.com":
			sawSM = true
		case ecsCredentialsHost:
		default:
			t.Fatalf("unexpected request host %q (all: %v)", h, after)
		}
	}
	if !sawSM {
		t.Fatalf("the Secrets Manager request must also go through the tripwire, got %v", after)
	}
}

// TestAWSSM_S1_StaticCredentialAliasesRefused (gate W2/W3 S-1): the SDK's
// alias variables are refused like the canonical ones, one at a time.
func TestAWSSM_S1_StaticCredentialAliasesRefused(t *testing.T) {
	for _, env := range []map[string]string{
		{"AWS_ACCESS_KEY": "AKIDEXAMPLE", "AWS_SECRET_KEY": "not-a-real-secret"},
		{"AWS_ACCESS_KEY": "AKIDEXAMPLE"},
		{"AWS_SECRET_KEY": "not-a-real-secret"},
		{"AWS_DEFAULT_PROFILE": "default"},
	} {
		t.Run(fmt.Sprint(env), func(t *testing.T) {
			cfg := permittedEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			_, err := New(context.Background(), cfg, "eu-west-1")
			if err == nil || !strings.Contains(err.Error(), "static AWS credential signal") {
				t.Fatalf("expected the static-credential refusal, got %v", err)
			}
		})
	}
}

// TestAWSSM_S1_ContainerProviderRequired: the container endpoint is the
// only authorized source, so its absence or a malformed relative URI
// refuses startup (never a fallback to another source).
func TestAWSSM_S1_ContainerProviderRequired(t *testing.T) {
	for _, rel := range []string{"", "@evil.example/x", "//evil.example/x", "/x?y=1", "/x#y", "http://evil.example/x", "/x@evil.example", "/x:1", "/ x"} {
		t.Run(rel, func(t *testing.T) {
			cfg := permittedEnv(t)
			t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", rel)
			if _, err := New(context.Background(), cfg, "eu-west-1"); err == nil || !strings.Contains(err.Error(), "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") {
				t.Fatalf("relative URI %q must refuse startup, got %v", rel, err)
			}
		})
	}
	cfg := permittedEnv(t)
	got, err := containerCredentialsEndpoint()
	if err != nil || got != "http://"+ecsCredentialsHost+testRelativeURI {
		t.Fatalf("endpoint = %q, %v", got, err)
	}
	_ = cfg
}

type fixedProvider struct {
	creds aws.Credentials
	err   error
}

func (p fixedProvider) Retrieve(context.Context) (aws.Credentials, error) { return p.creds, p.err }

// TestAWSSM_S1_UnauthorizedCredentialSourceRefused: a credential whose
// Source is not the container endpoint provider is refused on retrieval.
func TestAWSSM_S1_UnauthorizedCredentialSourceRefused(t *testing.T) {
	for _, src := range []string{"EnvConfigCredentials", "SharedConfigCredentials", "WebIdentityCredentials", "EC2RoleProvider", ""} {
		p := containerOnlyProvider{inner: fixedProvider{creds: aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "x", Source: src}}}
		if c, err := p.Retrieve(context.Background()); !errors.Is(err, errUnauthorizedCredentialSource) || c.AccessKeyID != "" {
			t.Fatalf("source %q must be refused, got %v %v", src, c.Source, err)
		}
	}
	ok := containerOnlyProvider{inner: fixedProvider{creds: aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "x", Source: endpointcreds.ProviderName}}}
	if c, err := ok.Retrieve(context.Background()); err != nil || c.Source != endpointcreds.ProviderName {
		t.Fatalf("the container provider must be accepted: %v", err)
	}
	inner := errors.New("inner failure")
	if _, err := (containerOnlyProvider{inner: fixedProvider{err: inner}}).Retrieve(context.Background()); !errors.Is(err, inner) {
		t.Fatalf("an inner error must pass through, got %v", err)
	}
}

// TestAWSSM_S2_EndpointTrustRootAndCredentialSourceOverridesRefused (gate
// W2/W3 S-2): each variable alone refuses startup, naming itself.
func TestAWSSM_S2_EndpointTrustRootAndCredentialSourceOverridesRefused(t *testing.T) {
	for _, k := range []string{
		"SSL_CERT_FILE", "SSL_CERT_DIR",
		"AWS_ENDPOINT_URL_STS", "AWS_ENDPOINT_URL_SSO", "AWS_ENDPOINT_URL_KMS",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_EC2_METADATA_SERVICE_ENDPOINT",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
	} {
		t.Run(k, func(t *testing.T) {
			cfg := permittedEnv(t)
			t.Setenv(k, "/tmp/or-https://evil.example")
			_, err := New(context.Background(), cfg, "eu-west-1")
			if err == nil || !strings.Contains(err.Error(), "("+k+")") {
				t.Fatalf("%s must refuse startup naming itself, got %v", k, err)
			}
			if _, err := NewWithSDKFake(context.Background(), cfg, "eu-west-1"); err == nil {
				t.Fatalf("NewWithSDKFake must apply the same refusal for %s", k)
			}
		})
	}
}

// hangingClient honours ctx and otherwise never answers.
type hangingClient struct{}

func (hangingClient) GetSecretValue(ctx context.Context, _ *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestAWSSM_HangingStoreBoundedThroughFetcher (code review #6): a Secrets
// Manager call that blocks past the 2 s budget comes back through
// Fetcher.Fetch as the unavailable class (the resolver's
// credential_store_unavailable) within the budget, not after the caller's
// much longer deadline.
func TestAWSSM_HangingStoreBoundedThroughFetcher(t *testing.T) {
	router, err := memstore.NewRouter(newStore(hangingClient{}))
	if err != nil {
		t.Fatal(err)
	}
	fetcher := secretstore.NewFetcher(router, func(b []byte) string { return "fp1:x" })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err = fetcher.Fetch(ctx, uuid.New(), validRef(t, ""), "fp1:x")
	elapsed := time.Since(start)
	if secretstore.ClassOf(err) != secretstore.ClassUnavailable {
		t.Fatalf("a hanging store must be unavailable, got %v", err)
	}
	// The §5 literal budget (2 s), not the constant, so a change to the
	// constant cannot silently widen this bound.
	const budget = 2 * time.Second
	if elapsed > budget+time.Second {
		t.Fatalf("Fetch took %s, want at most %s plus slack", elapsed, budget)
	}
}

// TestAWSSM_NewNeverCalledFromTestsOutsideThisPackage (code review #6): a
// test anywhere else must use NewWithSDKFake; only this package's tests,
// where TestMain installs the tripwire client, may call New. The only
// production caller is cmd/platform-api/registrations.go.
func TestAWSSM_NewNeverCalledFromTestsOutsideThisPackage(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	var prodCallers []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if n == "vendor" || n == "testdata" || n == "node_modules" || (strings.HasPrefix(n, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if !strings.HasSuffix(path, ".go") || strings.HasPrefix(rel, filepath.Join("internal", "secretstore", "awssm")+string(filepath.Separator)) {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		local := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "github.com/Diansalas/igaming-platform/internal/secretstore/awssm" {
				local = "awssm"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "New" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
				if strings.HasSuffix(path, "_test.go") {
					t.Errorf("%s references awssm.New from a test; use awssm.NewWithSDKFake", rel)
				} else {
					prodCallers = append(prodCallers, rel)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("cmd", "platform-api", "registrations.go")
	for _, c := range prodCallers {
		if c != want {
			t.Errorf("%s references awssm.New; only %s may", c, want)
		}
	}
}
