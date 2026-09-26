//go:build integration

// Stage 10.3 W2a: the provider-credential admin API (security review
// 07-w2a-design-review-security.md §6). Runs as the NOBYPASSRLS runtime
// role (TEST_RUNTIME_DATABASE_URL). Kept in its own file, away from the
// heavy Stage 9 concurrency tests (QA condition 6 / CI-FLAKE-281).
package httpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type pcAPI struct {
	t         *testing.T
	pool      *db.Pool
	issuer    *auth.Issuer
	srv       *httptest.Server
	mem       *memstore.Store
	logs      *syncBuffer
	requester uuid.UUID // platform admin, Person A
	approver  uuid.UUID // platform admin, Person B
	sameP     uuid.UUID // platform admin, Person A (second account)
}

func pcRandom(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func newPCAPI(t *testing.T, withSubsystem bool) *pcAPI {
	t.Helper()
	url := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping provider credential API test")
	}
	pool, err := db.Connect(context.Background(), url, 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": hex.EncodeToString(pcRandom(t, 32))})
	if err != nil {
		t.Fatal(err)
	}
	a := &pcAPI{t: t, pool: pool, issuer: auth.NewIssuer(keys, "pc-test", "pc-test"), mem: memstore.New(), logs: &syncBuffer{}}
	logger := slog.New(slog.NewJSONHandler(a.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var sub *providercred.Subsystem
	if withSubsystem {
		router, err := memstore.NewRouter(a.mem)
		if err != nil {
			t.Fatal(err)
		}
		sub, err = providercred.New(config.Config{ProviderCredentialFingerprintKey: config.NewSecretValue(hex.EncodeToString(pcRandom(t, 32)))},
			router, providercred.WithLogger(logger))
		if err != nil || sub == nil {
			t.Fatalf("subsystem: %v", err)
		}
	}
	a.srv = httptest.NewServer(New(Deps{
		Logger: logger, DB: pool, AuthIssuer: a.issuer, ServiceName: "pc-test",
		AccessTokenTTL: 5 * time.Minute, RefreshTokenTTL: time.Hour,
		PersonResolver:      identityresolution.NewMockPersonResolver(),
		ProviderCredentials: sub,
	}))
	t.Cleanup(a.srv.Close)
	pA := a.person()
	a.requester = a.platformStaff(&pA)
	a.approver = a.platformStaff(ptrUUID(a.person()))
	a.sameP = a.platformStaff(&pA)
	return a
}

func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }

func (a *pcAPI) person() uuid.UUID {
	id := uuid.New()
	if err := a.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, id)
		return err
	}); err != nil {
		a.t.Fatal(err)
	}
	return id
}

func (a *pcAPI) platformStaff(person *uuid.UUID) uuid.UUID {
	id := uuid.New()
	if err := a.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			VALUES ($1, NULL, $2, 'x', 'platform_admin', $3)`, id, "pcapi-"+id.String()+"@test.example", person)
		return err
	}); err != nil {
		a.t.Fatal(err)
	}
	return id
}

func (a *pcAPI) tenant() uuid.UUID {
	id := uuid.New()
	if err := a.pool.WithPlatformAdmin(context.Background(), a.requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, 'PC API', $2, 'under_platform_licence')`,
			id, "pcapi-"+strings.ReplaceAll(id.String(), "-", "")[:16])
		return err
	}); err != nil {
		a.t.Fatal(err)
	}
	return id
}

func (a *pcAPI) tenantStaff(tenant uuid.UUID, role string) uuid.UUID {
	id := uuid.New()
	if err := a.pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', $4)`,
			id, tenant, "pcapi-"+id.String()+"@test.example", role)
		return err
	}); err != nil {
		a.t.Fatal(err)
	}
	return id
}

func (a *pcAPI) token(subject uuid.UUID, tenant uuid.UUID, role auth.Role) string {
	tok, err := a.issuer.Issue(subject.String(), tenant, role, auth.PrincipalStaff, time.Hour)
	if err != nil {
		a.t.Fatal(err)
	}
	return tok
}

func (a *pcAPI) platformToken(subject uuid.UUID) string {
	return a.token(subject, uuid.Nil, auth.RolePlatformAdmin)
}

type apiResp struct {
	status int
	body   []byte
}

func (a *pcAPI) do(method, path, token string, body any) apiResp {
	a.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			a.t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.srv.URL+path, rdr)
	if err != nil {
		a.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return apiResp{status: resp.StatusCode, body: raw}
}

func (r apiResp) field(t *testing.T, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	v, _ := m[key].(string)
	return v
}

// regBody is a valid registration body for a fresh secret stored in the
// memory backend; it returns the body, the secret and the confirmation.
func (a *pcAPI) regBody(tenant uuid.UUID, provider, purpose, keyID string) (map[string]any, []byte, string) {
	secret := pcRandom(a.t, 32)
	ref := fmt.Sprintf("memory://api/provider-creds/%s/casino/%s/%s-%s?version=v1", tenant, provider, keyID, uuid.NewString()[:8])
	a.mem.Put(ref, secret)
	sum := sha256.Sum256(secret)
	confirmation := "sha256:" + hex.EncodeToString(sum[:])
	return map[string]any{
		"domain": "casino", "provider_id": provider, "purpose": purpose, "key_id": keyID,
		"secret_ref": ref, "confirmation": confirmation, "not_before": time.Now().Add(-time.Minute).Format(time.RFC3339Nano),
		"predecessor_disposition": "none", "reason_code": "initial_registration",
	}, secret, confirmation
}

func base(tenant uuid.UUID) string { return "/v1/admin/tenants/" + tenant.String() }

// activate runs file -> decide -> apply through the API; returns the
// handle id and request id.
func (a *pcAPI) activate(tenant uuid.UUID, body map[string]any) (handleID, requestID string) {
	a.t.Helper()
	filed := a.do("POST", base(tenant)+"/provider-credential-requests", a.platformToken(a.requester), body)
	if filed.status != http.StatusCreated {
		a.t.Fatalf("file: %d %s", filed.status, filed.body)
	}
	requestID = filed.field(a.t, "id")
	hash := filed.field(a.t, "content_hash")
	dec := a.do("POST", base(tenant)+"/provider-credential-requests/"+requestID+"/decisions", a.platformToken(a.approver),
		map[string]any{"decision": "approve", "content_hash": hash})
	if dec.status != http.StatusCreated {
		a.t.Fatalf("decide: %d %s", dec.status, dec.body)
	}
	app := a.do("POST", base(tenant)+"/provider-credential-requests/"+requestID+"/apply", a.platformToken(a.requester), nil)
	if app.status != http.StatusCreated {
		a.t.Fatalf("apply: %d %s", app.status, app.body)
	}
	return app.field(a.t, "id"), requestID
}

func (a *pcAPI) handleStatus(tenant uuid.UUID, id string) string {
	var s string
	if err := a.pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM provider_credential_handles WHERE id = $1`, id).Scan(&s)
	}); err != nil {
		a.t.Fatal(err)
	}
	return s
}

func TestProviderCredentialAPI_PermissionMatrix(t *testing.T) {
	a := newPCAPI(t, true)
	tenant := a.tenant()
	body, _, _ := a.regBody(tenant, "acme", "webhook_verify", "k1")
	handleID, requestID := a.activate(tenant, body)
	pending := a.do("POST", base(tenant)+"/provider-credential-requests", a.platformToken(a.requester), func() map[string]any {
		b, _, _ := a.regBody(tenant, "acme2", "webhook_verify", "k1")
		return b
	}())
	pendingID := pending.field(t, "id")

	type route struct {
		method, path string
		body         func() any
		allowed      map[auth.Role]bool
	}
	routes := []route{
		{"GET", base(tenant) + "/provider-credentials", nil, map[auth.Role]bool{auth.RolePlatformAdmin: true, auth.RoleTenantAdmin: true}},
		{"POST", base(tenant) + "/provider-credential-requests", func() any { b, _, _ := a.regBody(tenant, "acme3", "webhook_verify", "k1"); return b },
			map[auth.Role]bool{auth.RolePlatformAdmin: true}},
		{"GET", base(tenant) + "/provider-credential-requests", nil, map[auth.Role]bool{auth.RolePlatformAdmin: true}},
		{"GET", base(tenant) + "/provider-credential-requests/" + requestID, nil, map[auth.Role]bool{auth.RolePlatformAdmin: true}},
		{"POST", base(tenant) + "/provider-credential-requests/" + pendingID + "/decisions",
			func() any {
				return map[string]any{"decision": "reject", "content_hash": pending.field(t, "content_hash"), "reason_code": "matrix"}
			},
			map[auth.Role]bool{auth.RolePlatformAdmin: true}},
		{"POST", base(tenant) + "/provider-credential-requests/" + pendingID + "/apply", nil, map[auth.Role]bool{auth.RolePlatformAdmin: true}},
		{"POST", base(tenant) + "/provider-credentials/" + handleID + "/transitions",
			func() any {
				return map[string]any{"action": "shorten", "not_after": time.Now().Add(48 * time.Hour).Format(time.RFC3339), "reason_code": "vendor_instruction"}
			},
			map[auth.Role]bool{auth.RolePlatformAdmin: true, auth.RoleTenantAdmin: true}},
	}
	roles := []auth.Role{auth.RolePlatformAdmin, auth.RoleTenantAdmin, auth.RoleSupport, auth.RoleCompliance, auth.RoleFinance, auth.RoleRiskManager}
	for _, r := range routes {
		for _, role := range roles {
			t.Run(r.method+" "+r.path[len(base(tenant)):]+"/"+string(role), func(t *testing.T) {
				var tok string
				if role == auth.RolePlatformAdmin {
					tok = a.platformToken(a.approver) // the approver: distinct from the requester for decisions
				} else {
					tok = a.token(a.tenantStaff(tenant, staffRoleFor(role)), tenant, role)
				}
				var b any
				if r.body != nil {
					b = r.body()
				}
				resp := a.do(r.method, r.path, tok, b)
				if r.allowed[role] {
					if resp.status == http.StatusForbidden || resp.status == http.StatusUnauthorized {
						t.Fatalf("%s must be allowed, got %d %s", role, resp.status, resp.body)
					}
				} else if resp.status != http.StatusForbidden {
					t.Fatalf("%s must get 403, got %d %s", role, resp.status, resp.body)
				}
			})
		}
	}
	if resp := a.do("GET", base(tenant)+"/provider-credentials", "", nil); resp.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated must be 401, got %d", resp.status)
	}
}

// staffRoleFor maps a JWT role to the staff_users role string.
func staffRoleFor(r auth.Role) string { return string(r) }

func TestProviderCredentialAPI_TenantAdminCannotTargetOtherTenant(t *testing.T) {
	a := newPCAPI(t, true)
	ta, tb := a.tenant(), a.tenant()
	bodyA, _, _ := a.regBody(ta, "acme", "webhook_verify", "k1")
	handleA, _ := a.activate(ta, bodyA)
	bodyB, _, _ := a.regBody(tb, "acme", "webhook_verify", "k1")
	handleB, _ := a.activate(tb, bodyB)
	adminA := a.token(a.tenantStaff(ta, "tenant_admin"), ta, auth.RoleTenantAdmin)

	if resp := a.do("GET", base(tb)+"/provider-credentials", adminA, nil); resp.status != http.StatusForbidden || bytes.Contains(resp.body, []byte(handleB)) {
		t.Fatalf("listing another tenant must be 403 with no data, got %d %s", resp.status, resp.body)
	}
	revoke := map[string]any{"action": "revoke", "reason_code": "tenant_request"}
	if resp := a.do("POST", base(tb)+"/provider-credentials/"+handleB+"/transitions", adminA, revoke); resp.status != http.StatusForbidden {
		t.Fatalf("revoking another tenant's handle must be 403, got %d", resp.status)
	}
	// Naming B's handle under A's own path: invisible -> 404.
	if resp := a.do("POST", base(ta)+"/provider-credentials/"+handleB+"/transitions", adminA, revoke); resp.status != http.StatusNotFound {
		t.Fatalf("B's handle under A's path must be 404, got %d", resp.status)
	}
	if a.handleStatus(tb, handleB) != "active" || a.handleStatus(ta, handleA) != "active" {
		t.Fatal("no row may change in either tenant")
	}
}

func TestProviderCredentialAPI_TenantAdminCannotRequestApproveApply(t *testing.T) {
	a := newPCAPI(t, true)
	tenant := a.tenant()
	admin := a.token(a.tenantStaff(tenant, "tenant_admin"), tenant, auth.RoleTenantAdmin)
	body, _, _ := a.regBody(tenant, "acme", "webhook_verify", "k1")
	if resp := a.do("POST", base(tenant)+"/provider-credential-requests", admin, body); resp.status != http.StatusForbidden {
		t.Fatalf("file: %d", resp.status)
	}
	filed := a.do("POST", base(tenant)+"/provider-credential-requests", a.platformToken(a.requester), body)
	id := filed.field(t, "id")
	if resp := a.do("POST", base(tenant)+"/provider-credential-requests/"+id+"/decisions", admin,
		map[string]any{"decision": "approve", "content_hash": filed.field(t, "content_hash")}); resp.status != http.StatusForbidden {
		t.Fatalf("decide: %d", resp.status)
	}
	if resp := a.do("POST", base(tenant)+"/provider-credential-requests/"+id+"/apply", admin, nil); resp.status != http.StatusForbidden {
		t.Fatalf("apply: %d", resp.status)
	}
}

func TestProviderCredentialAPI_BodyTenantFieldRejected(t *testing.T) {
	a := newPCAPI(t, true)
	tenant := a.tenant()
	body, _, _ := a.regBody(tenant, "acme", "webhook_verify", "k1")
	body["tenant_id"] = a.tenant().String()
	resp := a.do("POST", base(tenant)+"/provider-credential-requests", a.platformToken(a.requester), body)
	if resp.status != http.StatusBadRequest || resp.field(t, "code") != "invalid_request" {
		t.Fatalf("a body tenant field must be 400 invalid_request, got %d %s", resp.status, resp.body)
	}
	for name, bad := range map[string]string{
		"malformed json": `{"domain":`,
		"unknown field":  `{"domain":"casino","extra":1}`,
	} {
		if resp := a.do("POST", base(tenant)+"/provider-credential-requests", a.platformToken(a.requester), bad); resp.status != http.StatusBadRequest {
			t.Fatalf("%s: got %d", name, resp.status)
		}
	}
}

var requestIDInBody = regexp.MustCompile(`"request_id":"[^"]*"`)

// TestProviderCredentialAPI_RegistrationErrorsUniform: every semantic
// registration failure returns one byte-identical 409 (apart from the
// request id), and the specific class goes to the log only.
func TestProviderCredentialAPI_RegistrationErrorsUniform(t *testing.T) {
	a := newPCAPI(t, true)
	tenant := a.tenant()
	other := a.tenant()
	existing, existingSecret, _ := a.regBody(tenant, "acme", "webhook_verify", "k1")
	a.activate(tenant, existing)
	tok := a.platformToken(a.requester)

	cases := map[string]func() map[string]any{
		"store_not_found": func() map[string]any {
			b, _, _ := a.regBody(tenant, "p1", "webhook_verify", "k1")
			b["secret_ref"] = fmt.Sprintf("memory://api/provider-creds/%s/casino/p1/absent?version=v1", tenant)
			return b
		},
		"store_access_denied": func() map[string]any {
			b, _, _ := a.regBody(tenant, "p2", "webhook_verify", "k1")
			a.mem.FailRef(b["secret_ref"].(string), secretstore.ClassAccessDenied)
			return b
		},
		"store_store_unavailable": func() map[string]any {
			b, _, _ := a.regBody(tenant, "p3", "webhook_verify", "k1")
			a.mem.FailRef(b["secret_ref"].(string), secretstore.ClassUnavailable)
			return b
		},
		"confirmation_mismatch": func() map[string]any {
			b, _, _ := a.regBody(tenant, "p4", "webhook_verify", "k1")
			b["confirmation"] = "sha256:" + hex.EncodeToString(pcRandom(t, 32))
			return b
		},
		"namespace_mismatch": func() map[string]any {
			b, secret, _ := a.regBody(tenant, "p5", "webhook_verify", "k1")
			ref := fmt.Sprintf("memory://api/provider-creds/%s/casino/p5/x?version=v1", other)
			a.mem.Put(ref, secret)
			b["secret_ref"] = ref
			return b
		},
		"backend_not_allowed": func() map[string]any {
			b, _, _ := a.regBody(tenant, "p6", "webhook_verify", "k1")
			b["secret_ref"] = fmt.Sprintf("devfile://provider-creds/%s/casino/p6/x?version=1", tenant)
			return b
		},
		"ref_invalid": func() map[string]any { // awssm without versionId
			b, _, _ := a.regBody(tenant, "p7", "webhook_verify", "k1")
			b["secret_ref"] = fmt.Sprintf("awssm://prefix/provider-creds/%s/casino/p7/x", tenant)
			return b
		},
		"duplicate_fingerprint": func() map[string]any {
			b, _, _ := a.regBody(tenant, "acme", "webhook_verify", "k9")
			ref := fmt.Sprintf("memory://api/provider-creds/%s/casino/acme/dup?version=v1", tenant)
			a.mem.Put(ref, existingSecret)
			sum := sha256.Sum256(existingSecret)
			b["secret_ref"], b["confirmation"] = ref, "sha256:"+hex.EncodeToString(sum[:])
			return b
		},
		"duplicate_key_id": func() map[string]any {
			b, _, _ := a.regBody(tenant, "acme", "webhook_verify", "k1")
			return b
		},
	}
	var first []byte
	for class, mk := range cases {
		resp := a.do("POST", base(tenant)+"/provider-credential-requests", tok, mk())
		if resp.status != http.StatusConflict {
			t.Fatalf("%s: want 409, got %d %s", class, resp.status, resp.body)
		}
		normalized := requestIDInBody.ReplaceAll(resp.body, []byte(`"request_id":""`))
		if first == nil {
			first = normalized
		} else if !bytes.Equal(first, normalized) {
			t.Fatalf("%s: body differs from the others:\n%s\n%s", class, first, normalized)
		}
		if !strings.Contains(a.logs.String(), `"class":"`+class+`"`) {
			t.Fatalf("%s: the specific class must be logged", class)
		}
	}
	if !bytes.Contains(first, []byte(`"code":"credential_registration_rejected"`)) {
		t.Fatalf("unexpected body %s", first)
	}
}

func TestProviderCredentialAPI_ApplyRechecksPrincipals(t *testing.T) {
	a := newPCAPI(t, true)
	tenant := a.tenant()
	body, _, _ := a.regBody(tenant, "acme", "webhook_verify", "k1")
	filed := a.do("POST", base(tenant)+"/provider-credential-requests", a.platformToken(a.requester), body)
	id := filed.field(t, "id")
	if resp := a.do("POST", base(tenant)+"/provider-credential-requests/"+id+"/decisions", a.platformToken(a.approver),
		map[string]any{"decision": "approve", "content_hash": filed.field(t, "content_hash")}); resp.status != http.StatusCreated {
		t.Fatalf("decide: %d %s", resp.status, resp.body)
	}
	if err := a.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, a.approver)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := a.do("POST", base(tenant)+"/provider-credential-requests/"+id+"/apply", a.platformToken(a.requester), nil)
	if resp.status != http.StatusConflict || resp.field(t, "code") != "credential_activation_rejected" {
		t.Fatalf("a suspended approver must make apply 409, got %d %s", resp.status, resp.body)
	}
}

func TestProviderCredentialAPI_DecisionErrors(t *testing.T) {
	a := newPCAPI(t, true)
	tenant := a.tenant()
	body, _, _ := a.regBody(tenant, "acme", "webhook_verify", "k1")
	filed := a.do("POST", base(tenant)+"/provider-credential-requests", a.platformToken(a.requester), body)
	id, hash := filed.field(t, "id"), filed.field(t, "content_hash")
	path := base(tenant) + "/provider-credential-requests/" + id + "/decisions"
	for name, tc := range map[string]struct {
		subject uuid.UUID
		hash    string
	}{
		"self-approval": {a.requester, hash},
		"same person":   {a.sameP, hash},
		"hash mismatch": {a.approver, hex.EncodeToString(pcRandom(t, 32))},
	} {
		resp := a.do("POST", path, a.platformToken(tc.subject), map[string]any{"decision": "approve", "content_hash": tc.hash})
		if resp.status != http.StatusConflict || resp.field(t, "code") != "approval_rejected" {
			t.Fatalf("%s: want 409 approval_rejected, got %d %s", name, resp.status, resp.body)
		}
		if bytes.Contains(resp.body, []byte("provider_credential")) {
			t.Fatalf("%s: trigger text leaked: %s", name, resp.body)
		}
	}
	if resp := a.do("POST", base(a.tenant())+"/provider-credential-requests/"+id+"/decisions", a.platformToken(a.approver),
		map[string]any{"decision": "approve", "content_hash": hash}); resp.status != http.StatusNotFound {
		t.Fatalf("a request under another tenant's path must be 404, got %d", resp.status)
	}
}

func TestProviderCredentialAPI_TransitionErrors(t *testing.T) {
	a := newPCAPI(t, true)
	tenant := a.tenant()
	body, _, _ := a.regBody(tenant, "acme", "webhook_verify", "k1")
	handle, _ := a.activate(tenant, body)
	path := base(tenant) + "/provider-credentials/" + handle + "/transitions"
	tok := a.platformToken(a.requester)
	for name, tc := range map[string]struct {
		body map[string]any
		want int
		code string
	}{
		"missing reason": {map[string]any{"action": "revoke"}, 400, "invalid_request"},
		"unknown reason": {map[string]any{"action": "revoke", "reason_code": "because"}, 400, "invalid_request"},
		"unknown action": {map[string]any{"action": "reactivate", "reason_code": "rotation"}, 400, "invalid_request"},
		"cap exceeded":   {map[string]any{"action": "verify_only", "reason_code": "rotation", "not_after": time.Now().Add(8 * 24 * time.Hour).Format(time.RFC3339)}, 409, "credential_transition_rejected"},
		"extension":      {map[string]any{"action": "shorten", "reason_code": "vendor_instruction", "not_after": time.Now().Add(-time.Hour).Format(time.RFC3339)}, 409, "credential_transition_rejected"},
	} {
		resp := a.do("POST", path, tok, tc.body)
		if resp.status != tc.want || resp.field(t, "code") != tc.code {
			t.Fatalf("%s: want %d %s, got %d %s", name, tc.want, tc.code, resp.status, resp.body)
		}
	}
	if resp := a.do("POST", path, tok, map[string]any{"action": "revoke", "reason_code": "misregistration"}); resp.status != http.StatusOK {
		t.Fatalf("revoke: %d %s", resp.status, resp.body)
	}
	if resp := a.do("POST", path, tok, map[string]any{"action": "revoke", "reason_code": "misregistration"}); resp.status != http.StatusConflict {
		t.Fatalf("a revoked row is terminal: %d %s", resp.status, resp.body)
	}
	if resp := a.do("POST", base(tenant)+"/provider-credentials/"+uuid.NewString()+"/transitions", tok,
		map[string]any{"action": "revoke", "reason_code": "misregistration"}); resp.status != http.StatusNotFound {
		t.Fatalf("an unknown handle must be 404, got %d", resp.status)
	}
}

func TestProviderCredentialAPI_RevokeSingleActorTenantAndPlatform(t *testing.T) {
	a := newPCAPI(t, true)
	tenant := a.tenant()
	b1, _, _ := a.regBody(tenant, "acme", "webhook_verify", "k1")
	h1, _ := a.activate(tenant, b1)
	b2, _, _ := a.regBody(tenant, "acme2", "webhook_verify", "k1")
	h2, _ := a.activate(tenant, b2)
	adminTok := a.token(a.tenantStaff(tenant, "tenant_admin"), tenant, auth.RoleTenantAdmin)
	if resp := a.do("POST", base(tenant)+"/provider-credentials/"+h1+"/transitions", adminTok,
		map[string]any{"action": "revoke", "reason_code": "tenant_request"}); resp.status != http.StatusOK {
		t.Fatalf("tenant admin revoke: %d %s", resp.status, resp.body)
	}
	if resp := a.do("POST", base(tenant)+"/provider-credentials/"+h2+"/transitions", a.platformToken(a.approver),
		map[string]any{"action": "revoke", "reason_code": "suspected_compromise"}); resp.status != http.StatusOK {
		t.Fatalf("platform admin revoke: %d %s", resp.status, resp.body)
	}
	if a.handleStatus(tenant, h1) != "revoked" || a.handleStatus(tenant, h2) != "revoked" {
		t.Fatal("both revocations must take effect with a single actor")
	}
}

type auditRow struct {
	tenant     *uuid.UUID
	actor      uuid.UUID
	action     string
	targetType string
	targetID   string
	outcome    string
	ip         string
	requestID  string
	metadata   map[string]any
}

func (a *pcAPI) auditRows(tenant uuid.UUID) []auditRow {
	var out []auditRow
	read := func(ctx context.Context, tx pgx.Tx, sql string) error {
		rows, err := tx.Query(ctx, sql, tenant.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r auditRow
			var ip *string
			var md []byte
			if err := rows.Scan(&r.tenant, &r.actor, &r.action, &r.targetType, &r.targetID, &r.outcome, &ip, &r.requestID, &md); err != nil {
				return err
			}
			if ip != nil {
				r.ip = *ip
			}
			_ = json.Unmarshal(md, &r.metadata)
			out = append(out, r)
		}
		return rows.Err()
	}
	const cols = `SELECT tenant_id, actor_id, action, coalesce(target_type,''), coalesce(target_id,''), outcome, host(ip_address), coalesce(request_id,''), metadata FROM audit_log `
	if err := a.pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return read(ctx, tx, cols+`WHERE tenant_id = $1::uuid AND action LIKE 'provider_credential.%' ORDER BY created_at`)
	}); err != nil {
		a.t.Fatal(err)
	}
	if err := a.pool.WithPlatformAdmin(context.Background(), a.requester, func(ctx context.Context, tx pgx.Tx) error {
		return read(ctx, tx, cols+`WHERE tenant_id IS NULL AND metadata->>'target_tenant_id' = $1 AND action LIKE 'provider_credential.%' ORDER BY created_at`)
	}); err != nil {
		a.t.Fatal(err)
	}
	return out
}

func TestProviderCredentialAPI_AuditRowPerWrite(t *testing.T) {
	a := newPCAPI(t, true)
	tenant := a.tenant()
	b1, _, _ := a.regBody(tenant, "acme", "webhook_verify", "k1")
	h1, _ := a.activate(tenant, b1)
	// Rotation with a verify_only predecessor: the apply writes TWO rows
	// (predecessor_transitioned + activated).
	b2, _, _ := a.regBody(tenant, "acme", "webhook_verify", "k2")
	b2["predecessor_handle_id"], b2["predecessor_disposition"] = h1, "verify_only"
	b2["predecessor_not_after"], b2["reason_code"] = time.Now().Add(24*time.Hour).Format(time.RFC3339Nano), "scheduled_rotation"
	h2, _ := a.activate(tenant, b2)
	if resp := a.do("POST", base(tenant)+"/provider-credentials/"+h2+"/transitions", a.platformToken(a.requester),
		map[string]any{"action": "revoke", "reason_code": "misregistration"}); resp.status != http.StatusOK {
		t.Fatalf("revoke: %d", resp.status)
	}
	rows := a.auditRows(tenant)
	count := map[string]int{}
	for _, r := range rows {
		count[r.action]++
		if r.outcome != "success" || r.ip == "" || r.requestID == "" {
			t.Fatalf("row %s lacks ip/request id/outcome: %+v", r.action, r)
		}
		if r.metadata["target_tenant_id"] != tenant.String() || r.metadata["fingerprint"] == nil || r.metadata["secret_ref"] == nil {
			t.Fatalf("row %s lacks required metadata: %v", r.action, r.metadata)
		}
		switch r.action {
		case "provider_credential.request_filed", "provider_credential.request_decided":
			if r.tenant != nil || r.targetType != "provider_credential_request" {
				t.Fatalf("%s must be a platform-scope request row: %+v", r.action, r)
			}
		default:
			if r.tenant == nil || *r.tenant != tenant || r.targetType != "provider_credential_handle" {
				t.Fatalf("%s must be a tenant-scope handle row: %+v", r.action, r)
			}
		}
		if r.action == "provider_credential.predecessor_transitioned" {
			if r.targetID != h1 || r.metadata["status_before"] != "active" || r.metadata["status_after"] != "verify_only" || r.metadata["reason_code"] != "rotation" {
				t.Fatalf("predecessor row: %+v", r)
			}
		}
	}
	for action, want := range map[string]int{
		"provider_credential.request_filed": 2, "provider_credential.request_decided": 2,
		"provider_credential.activated": 2, "provider_credential.predecessor_transitioned": 1,
		"provider_credential.transitioned": 1,
	} {
		if count[action] != want {
			t.Fatalf("%s rows = %d, want %d (all: %v)", action, count[action], want, count)
		}
	}
}

// TestProviderCredentialAPI_ResponsesAuditsLogsNeverContainSecret scans
// every response, audit row and log line for the raw, hex, base64 and
// base64url forms of a sentinel secret and of the confirmation value.
func TestProviderCredentialAPI_ResponsesAuditsLogsNeverContainSecret(t *testing.T) {
	a := newPCAPI(t, true)
	tenant := a.tenant()
	body, secret, confirmation := a.regBody(tenant, "acme", "webhook_verify", "k1")
	var responses [][]byte
	filed := a.do("POST", base(tenant)+"/provider-credential-requests", a.platformToken(a.requester), body)
	responses = append(responses, filed.body)
	id := filed.field(t, "id")
	for _, r := range []apiResp{
		a.do("POST", base(tenant)+"/provider-credential-requests/"+id+"/decisions", a.platformToken(a.approver),
			map[string]any{"decision": "approve", "content_hash": filed.field(t, "content_hash")}),
		a.do("GET", base(tenant)+"/provider-credential-requests/"+id, a.platformToken(a.requester), nil),
		a.do("GET", base(tenant)+"/provider-credential-requests", a.platformToken(a.requester), nil),
		a.do("POST", base(tenant)+"/provider-credential-requests/"+id+"/apply", a.platformToken(a.requester), nil),
		a.do("GET", base(tenant)+"/provider-credentials", a.platformToken(a.requester), nil),
	} {
		responses = append(responses, r.body)
	}
	// A rejected registration (confirmation mismatch) too.
	bad := map[string]any{}
	for k, v := range body {
		bad[k] = v
	}
	bad["key_id"] = "k2"
	bad["confirmation"] = confirmation[:len(confirmation)-1] + "0"
	responses = append(responses, a.do("POST", base(tenant)+"/provider-credential-requests", a.platformToken(a.requester), bad).body)

	forms := map[string]string{
		"raw": string(secret), "hex": hex.EncodeToString(secret), "HEX": strings.ToUpper(hex.EncodeToString(secret)),
		"base64": base64.StdEncoding.EncodeToString(secret), "base64url": base64.RawURLEncoding.EncodeToString(secret),
		"confirmation": confirmation, "confirmation-hex": strings.TrimPrefix(confirmation, "sha256:"),
	}
	var audit []byte
	for _, r := range a.auditRows(tenant) {
		raw, _ := json.Marshal(r.metadata)
		audit = append(audit, raw...)
	}
	for name, form := range forms {
		for _, resp := range responses {
			if bytes.Contains(resp, []byte(form)) {
				t.Fatalf("a response contains the %s form: %s", name, resp)
			}
		}
		if bytes.Contains(audit, []byte(form)) {
			t.Fatalf("an audit row contains the %s form", name)
		}
		if strings.Contains(a.logs.String(), form) {
			t.Fatalf("a log line contains the %s form", name)
		}
	}
}

// TestProviderCredentialAPI_RoutesNotMountedWithoutSubsystem: with no
// fingerprint key (subsystem nil) the request/decision/apply routes do not
// exist, while listing and single-actor revocation remain available.
func TestProviderCredentialAPI_RoutesNotMountedWithoutSubsystem(t *testing.T) {
	a := newPCAPI(t, false)
	tenant := a.tenant()
	tok := a.platformToken(a.requester)
	for _, path := range []string{"/provider-credential-requests", "/provider-credential-requests/" + uuid.NewString() + "/decisions",
		"/provider-credential-requests/" + uuid.NewString() + "/apply"} {
		resp := a.do("POST", base(tenant)+path, tok, map[string]any{})
		if resp.status != http.StatusNotFound && resp.status != http.StatusMethodNotAllowed {
			t.Fatalf("%s must not be mounted, got %d", path, resp.status)
		}
	}
	if resp := a.do("GET", base(tenant)+"/provider-credentials", tok, nil); resp.status != http.StatusOK {
		t.Fatalf("listing must stay available, got %d", resp.status)
	}
	if resp := a.do("POST", base(tenant)+"/provider-credentials/"+uuid.NewString()+"/transitions", tok,
		map[string]any{"action": "revoke", "reason_code": "misregistration"}); resp.status != http.StatusNotFound {
		t.Fatalf("the transition route must stay mounted (404 for an unknown handle), got %d", resp.status)
	}
}
