//go:build integration

package alerting

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Security delta finding 2: the three "is this kind human" sources must not
// diverge. (1) the database kind vocabulary (the channel_kind CHECKs), (2) the
// database function alerting_channel_kind_is_human_notification, (3) the Go
// functions. Today the vocabulary is exactly {log, mock}, both non-human, and
// the display allow-list is empty. When a real kind is added to the CHECKs, this
// test fails until the display list (knownHumanChannelKinds) names it too, so a
// real page can never be displayed as "recorded_non_human".
func TestHumanKindSources_AgreeWithTheDatabaseVocabulary(t *testing.T) {
	pool := scratchPool(t, "ahk")
	quoted := regexp.MustCompile(`'([a-z0-9_]+)'`)
	vocab := func(constraint, table string) []string {
		var def string
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c JOIN pg_class r ON r.oid = c.conrelid
				WHERE c.conname = $1 AND r.relname = $2`, constraint, table).Scan(&def)
		}); err != nil {
			t.Fatalf("read %s: %v", constraint, err)
		}
		var out []string
		for _, m := range quoted.FindAllStringSubmatch(def, -1) {
			out = append(out, m[1])
		}
		sort.Strings(out)
		return out
	}
	routes := vocab("alert_routes_channel_kind_check", "alert_routes")
	deliveries := vocab("alert_deliveries_channel_kind_check", "alert_deliveries")
	if strings.Join(routes, ",") != strings.Join(deliveries, ",") || len(routes) == 0 {
		t.Fatalf("the two channel_kind vocabularies differ: routes %v, deliveries %v", routes, deliveries)
	}

	// vocabulary minus the known non-human kinds == the display allow-list keys
	var extra []string
	for _, k := range routes {
		if k != string(ChannelLog) && k != string(ChannelMock) {
			extra = append(extra, k)
		}
	}
	var keys []string
	for k := range knownHumanChannelKinds {
		keys = append(keys, string(k))
	}
	sort.Strings(extra)
	sort.Strings(keys)
	if strings.Join(extra, ",") != strings.Join(keys, ",") {
		t.Fatalf("DB kinds beyond log/mock %v != knownHumanChannelKinds %v: a real kind must be registered for display too", extra, keys)
	}

	// the Go and DB human checks agree on every vocabulary kind and on unknown ones
	for _, k := range append(append([]string{}, routes...), "pager", "sms", "") {
		var dbHuman bool
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT alerting_channel_kind_is_human_notification($1)`, k).Scan(&dbHuman)
		}); err != nil {
			t.Fatal(err)
		}
		if goHuman := ChannelKindIsHumanNotification(ChannelKind(k)); goHuman != dbHuman {
			t.Errorf("kind %q: Go says human=%v, database says %v", k, goHuman, dbHuman)
		}
	}
}
