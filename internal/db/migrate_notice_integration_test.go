//go:build integration

package db

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// ADR 0118 security review F2: a pool opened with ConnectWithNoticeHandler delivers every server notice
// raised while MigrateUp applies a migration (severity + primary message); a plain Connect pool still
// applies the same migration (the handler is opt-in and changes nothing else).
func TestMigrateUp_NoticeHandlerReceivesRaiseNotice(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writeMigrationPair(t, dir, 1, "notice_probe",
		`DO $$ BEGIN RAISE NOTICE 'f2 probe % %', 'hold=1', 'cash=0'; END $$; CREATE TABLE notice_probe_t (id int);`,
		`DROP TABLE notice_probe_t;`)

	var mu sync.Mutex
	var got []string
	pool, err := ConnectWithNoticeHandler(ctx, checksumScratchDatabase(t), 2, 5*time.Second, func(sev, msg string) {
		mu.Lock()
		got = append(got, sev+"|"+msg)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if applied, err := pool.MigrateUp(ctx, dir); err != nil || len(applied) != 1 {
		t.Fatalf("MigrateUp: %v %v", applied, err)
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, g := range got {
		if g == "NOTICE|f2 probe hold=1 cash=0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the migration's RAISE NOTICE must reach the handler (severity|message), got %q", got)
	}
	if strings.Contains(strings.Join(got, "\n"), "PL/pgSQL") {
		t.Fatalf("only the primary message is delivered (no context): %q", got)
	}

	// Non-vacuity of the opt-in: a plain Connect pool applies the same migration with no handler.
	plain := checksumScratchPool(t, checksumScratchDatabase(t))
	if applied, err := plain.MigrateUp(ctx, dir); err != nil || len(applied) != 1 {
		t.Fatalf("plain MigrateUp: %v %v", applied, err)
	}
}
