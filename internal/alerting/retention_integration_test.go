//go:build integration

package alerting_test

import (
	"context"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertworld"
)

// The Go entry point (Dispatcher.PurgeResolved) reaches the migration-0157 function through the real runtime pool.
func TestDispatcherPurgeResolved_ThroughTheRuntimeRole(t *testing.T) {
	w := alertworld.NewWorld(t, "m157_go")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	seedHistory(t, w, admin, tenant, 1, true, 400*day)
	keep := seedHistory(t, w, admin, tenant, 0, false, 0)
	d := alerting.NewDispatcher(w.Pool, alerting.DefaultDispatcherConfig(), alerting.LogSink{})
	r, err := d.PurgeResolved(context.Background(), 90*day, 50)
	if err != nil {
		t.Fatal(err)
	}
	if r.Alerts != 1 || r.Occurrences != 2 || r.Deliveries != 1 || r.Capped {
		t.Fatalf("result = %+v", r)
	}
	if count(t, w, admin, `alerts WHERE id = '`+keep.String()+`'`) != 1 {
		t.Fatal("the open alert was removed")
	}
	// a window below the floor is refused by the database even if a caller skipped the Go validation
	if _, err := d.PurgeResolved(context.Background(), 24*time.Hour, 50); pgCode(err) != "AR203" {
		t.Fatalf("a 1-day window must be refused by the database (AR203), got %v", err)
	}
	// and a cancelled context is an error, not a silent success
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.PurgeResolved(ctx, 90*day, 50); err == nil {
		t.Fatal("a cancelled context purged")
	}
}
