package httpserver

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/marketgate"
)

// A8 DB-free contract pins: the three gated operations document the generic
// 403 MARKET_NOT_AVAILABLE refusal, the code is in the Error enum, and the
// shared response states the fixed player-facing message.

func opBlock(t *testing.T, spec, path, method string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(path) + `:\n`)
	loc := re.FindStringIndex(spec)
	if loc == nil {
		t.Fatalf("path %s is not documented", path)
	}
	rest := spec[loc[1]:]
	if next := regexp.MustCompile(`(?m)^  \S`).FindStringIndex(rest); next != nil {
		rest = rest[:next[0]]
	}
	mre := regexp.MustCompile(`(?m)^    ` + method + `:\n`)
	mloc := mre.FindStringIndex(rest)
	if mloc == nil {
		t.Fatalf("%s %s is not documented", strings.ToUpper(method), path)
	}
	block := rest[mloc[1]:]
	if next := regexp.MustCompile(`(?m)^    \S`).FindStringIndex(block); next != nil {
		block = block[:next[0]]
	}
	return block
}

func TestOpenAPI_MarketGate_ThreeOperationsDocumentTheGenericRefusal(t *testing.T) {
	spec := readSpec(t)
	for _, op := range []struct{ path, method string }{
		{"/v1/auth/register", "post"},
		{"/v1/me/deposits", "post"},
		{"/v1/me/casino/games/{gameID}/launch", "post"},
	} {
		block := opBlock(t, spec, op.path, op.method)
		if !regexp.MustCompile(`(?m)^        "403":\n          \$ref: "#/components/responses/MarketNotAvailable"`).MatchString(block) {
			t.Errorf("%s %s must document 403 via #/components/responses/MarketNotAvailable", strings.ToUpper(op.method), op.path)
		}
	}
}

func TestOpenAPI_MarketGate_ErrorEnumAndSharedResponse(t *testing.T) {
	spec := readSpec(t)
	if !strings.Contains(spec, "\n            - "+string(apierror.CodeMarketNotAvailable)+"\n") {
		t.Errorf("Error.code enum must list %s", apierror.CodeMarketNotAvailable)
	}
	i := strings.Index(spec, "\n    MarketNotAvailable:\n")
	if i < 0 {
		t.Fatal("components.responses.MarketNotAvailable is missing")
	}
	block := spec[i+len("\n    MarketNotAvailable:\n"):]
	if next := regexp.MustCompile(`(?m)^    \S`).FindStringIndex(block); next != nil {
		block = block[:next[0]]
	}
	for _, want := range []string{"MARKET_NOT_AVAILABLE", marketgate.PlayerMessage, "fail closed", "NEVER"} {
		if !strings.Contains(block, want) {
			t.Errorf("the MarketNotAvailable response must mention %q", want)
		}
	}
}
