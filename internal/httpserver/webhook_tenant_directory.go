package httpserver

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// webhookTenantDirectory is ADR 0097 §4.2's in-memory, copy-on-write
// snapshot of active tenant slugs. It exists ONLY to choose a bounded
// pre-auth rate-limiter key (preKey) - it is non-authoritative (architect
// review AC3): it exposes Contains only (no tenant id, status beyond
// "active", or licensing model), and no route other than the webhook
// admission layer may use it. Authority over whether a slug names a real,
// active tenant stays entirely with identity.GetTenantBySlug plus the
// active check plus signature verification - a suspended tenant still in
// a stale snapshot gets its own limiter bucket and then the ordinary
// uniform 401; a brand-new tenant is keyed "_unknown" for at most one
// refresh interval.
//
// Deliberately unexported: no other package, and no other route in this
// package, may reach into it.
type webhookTenantDirectory struct {
	snapshot atomic.Pointer[map[string]struct{}]
	loadedAt atomic.Pointer[time.Time]

	pool  tenantDirectoryPool
	limit int
	log   *slog.Logger
}

// tenantDirectoryPool is the minimal dependency the directory needs -
// kept as an interface so tests can fake it without a real database.
type tenantDirectoryPool interface {
	ListActiveTenantSlugs(ctx context.Context, limit int) ([]string, error)
}

// identityPoolAdapter adapts *db.Pool (via identity.ListActiveTenantSlugs)
// to tenantDirectoryPool.
type identityPoolAdapter struct{ db *db.Pool }

func (a identityPoolAdapter) ListActiveTenantSlugs(ctx context.Context, limit int) ([]string, error) {
	return identity.ListActiveTenantSlugs(ctx, a.db, limit)
}

// newWebhookTenantDirectory constructs the directory. It does NOT load
// anything yet - call Load synchronously once at startup (ADR 0097 §7
// "startup waits for the first load").
func newWebhookTenantDirectory(pool tenantDirectoryPool, limit int, log *slog.Logger) *webhookTenantDirectory {
	if log == nil {
		log = slog.Default()
	}
	return &webhookTenantDirectory{pool: pool, limit: limit, log: log}
}

// Contains reports whether slug is in the last successfully loaded
// snapshot. A nil snapshot (never loaded) reports false for every slug -
// callers must check Loaded() before trusting a false as "not a tenant"
// vs "directory not ready" (New's caller gates /readyz and the webhook
// routes on Loaded()).
func (d *webhookTenantDirectory) Contains(slug string) bool {
	snap := d.snapshot.Load()
	if snap == nil {
		return false
	}
	_, ok := (*snap)[slug]
	return ok
}

// Loaded reports whether the directory has completed at least one
// successful load.
func (d *webhookTenantDirectory) Loaded() bool {
	return d.snapshot.Load() != nil
}

// Age returns how long ago the current snapshot was loaded, and false if
// never loaded (for the webhook_tenant_directory_age_seconds gauge/alert,
// ADR 0097 §8/§7).
func (d *webhookTenantDirectory) Age(now time.Time) (time.Duration, bool) {
	t := d.loadedAt.Load()
	if t == nil {
		return 0, false
	}
	return now.Sub(*t), true
}

// Size returns the number of slugs in the current snapshot (0 if never
// loaded), for the webhook_tenant_directory_size gauge.
func (d *webhookTenantDirectory) Size() int {
	snap := d.snapshot.Load()
	if snap == nil {
		return 0
	}
	return len(*snap)
}

// Load performs one synchronous refresh. On a query error it KEEPS the
// last snapshot (ADR 0097 §7 "refresh fails: keep last snapshot") and
// returns the error for the caller to log; it never falls back to
// treating every slug as known, and never wipes a good snapshot with an
// empty one because of a transient failure.
func (d *webhookTenantDirectory) Load(ctx context.Context) error {
	slugs, err := d.pool.ListActiveTenantSlugs(ctx, d.limit)
	if err != nil {
		return err
	}
	if len(slugs) >= d.limit {
		// ADR 0097 §4.2: at the cap, one error log per refresh - this is
		// misconfiguration territory (cap should exceed the real tenant
		// count), not a normal state.
		d.log.Error("webhook_tenant_directory_truncated", "cap", d.limit)
	}
	m := make(map[string]struct{}, len(slugs))
	for _, s := range slugs {
		m[s] = struct{}{}
	}
	now := time.Now()
	d.snapshot.Store(&m)
	d.loadedAt.Store(&now)
	return nil
}

// Run refreshes the directory every interval until ctx is cancelled
// (ADR 0097 §7/§17 devops condition 2: wired to the SAME shutdown context
// as http.Server.Shutdown, so it stops cleanly and never logs after the
// logger/DB pool is torn down). Run does its own periodic loads only - the
// caller is responsible for the synchronous initial Load() before serving
// any traffic.
func (d *webhookTenantDirectory) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.Load(ctx); err != nil {
				if ctx.Err() != nil {
					return // shutting down; do not log after teardown
				}
				d.log.Error("webhook_tenant_directory_refresh_failed", "error", err)
			}
		}
	}
}
