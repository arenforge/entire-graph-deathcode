package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

// The risk verdict is the one number-shaped thing this command prints, so it is
// the one a reader is most likely to quote out of context. These cases pin the
// boundaries so a later change cannot quietly turn MEDIUM into HIGH.
func TestSimulateRiskVerdicts(t *testing.T) {
	cases := []struct {
		name       string
		affected   int
		uncovered  int
		wantRisk   string
		wantReason string
	}{
		{"no blast radius", 0, 0, "NONE", "no affected symbols resolved for this focus"},
		{"fully covered", 8, 0, "LOW", "all 8 affected symbols have a covering test"},
		{"one gap in many", 20, 1, "MEDIUM", "1 of 20 affected symbols have no covering test"},
		{"majority uncovered below count threshold", 4, 2, "HIGH", "2 of 4 affected symbols have no covering test (majority uncovered)"},
		{"count threshold fires", 40, 5, "HIGH", "5 of 40 affected symbols have no covering test (>= 5 uncovered)"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			risk, reason := simulateRisk(testCase.affected, testCase.uncovered)
			if risk != testCase.wantRisk {
				t.Errorf("risk = %q, want %q", risk, testCase.wantRisk)
			}
			if reason != testCase.wantReason {
				t.Errorf("reason = %q, want %q", reason, testCase.wantReason)
			}
		})
	}
}

// A stem mismatch silently disables the mirror tier, which would look like
// "no test covers this" rather than like a bug. These pairs must collapse.
func TestSimulateFileStemPairsSourceWithTest(t *testing.T) {
	pairs := [][2]string{
		{"internal/cli/impact.go", "internal/cli/impact_test.go"},
		{"src/auth/login.ts", "src/auth/login.test.ts"},
		{"app/services/session.py", "app/services/test_session.py"},
		{"lib/parser.rb", "lib/parser_spec.rb"},
	}
	for _, pair := range pairs {
		source, test := simulateFileStem(pair[0]), simulateFileStem(pair[1])
		if source == "" {
			t.Errorf("simulateFileStem(%q) is empty", pair[0])
		}
		if source != test {
			t.Errorf("simulateFileStem(%q) = %q, simulateFileStem(%q) = %q; want equal",
				pair[0], source, pair[1], test)
		}
	}
}

// Truncation must not hide the finding: uncovered entries are the reason to run
// the command, so they survive the cap ahead of covered ones.
func TestCapSimulateAffectedKeepsUncoveredFirst(t *testing.T) {
	affected := []simulateAffected{
		{Endpoint: neighborEndpoint{Name: "covered1"}, Tests: []simulateCoveringTest{{Evidence: simulateEvidenceEdge}}},
		{Endpoint: neighborEndpoint{Name: "uncovered1"}},
		{Endpoint: neighborEndpoint{Name: "covered2"}, Tests: []simulateCoveringTest{{Evidence: simulateEvidenceEdge}}},
		{Endpoint: neighborEndpoint{Name: "uncovered2"}},
	}
	kept := capSimulateAffected(affected, 2)
	if len(kept) != 2 {
		t.Fatalf("kept %d entries, want 2", len(kept))
	}
	for _, entry := range kept {
		if len(entry.Tests) != 0 {
			t.Errorf("kept covered entry %q ahead of an uncovered one", entry.Endpoint.Name)
		}
	}
}

// Tests are the instrument, not the subject: a test symbol appearing in the
// blast radius must never be reported as uncovered production code.
func TestCollectSimulateAffectedDropsTestsAndExternals(t *testing.T) {
	focus := neighborEndpoint{ID: "focus", Name: "Login", FilePath: "auth/login.go", StartLine: 10}
	impact := impactResponse{
		Focus: &focus,
		Callers: impactSection{Entries: []impactEntry{
			{Endpoint: neighborEndpoint{ID: "t1", Name: "TestLogin", FilePath: "auth/login_test.go"}, Relation: "CALLS"},
			{Endpoint: neighborEndpoint{ID: "ext", Name: "Vendor", FilePath: "vendor/x.go", External: true}, Relation: "CALLS"},
			{Endpoint: neighborEndpoint{ID: "mw", Name: "Authorize", FilePath: "auth/mw.go", StartLine: 31}, Relation: "CALLS"},
			{Endpoint: neighborEndpoint{ID: "mw", Name: "Authorize", FilePath: "auth/mw.go", StartLine: 31}, Relation: "CALLS"},
		}},
		// Callees cannot break when the focus changes, so they must not appear.
		Callees: impactSection{Entries: []impactEntry{
			{Endpoint: neighborEndpoint{ID: "callee", Name: "Hash", FilePath: "auth/hash.go"}, Relation: "CALLS"},
		}},
	}
	affected := collectSimulateAffected(impact)
	if len(affected) != 2 {
		t.Fatalf("affected = %d entries, want 2 (focus + Authorize); got %+v", len(affected), affected)
	}
	if !affected[0].Focus || affected[0].Endpoint.Name != "Login" {
		t.Errorf("first entry = %+v, want the focus", affected[0])
	}
	if affected[1].Endpoint.Name != "Authorize" {
		t.Errorf("second entry = %q, want Authorize", affected[1].Endpoint.Name)
	}
	if affected[1].Module != "auth" {
		t.Errorf("module = %q, want auth", affected[1].Module)
	}
}

// Cross-module callers are the ones a developer reading their own diff will not
// notice, so the classification must not depend on the order sections arrive in
// or credit the focus itself as breaking.
func TestCollectSimulateAffectedMarksCrossModule(t *testing.T) {
	focus := neighborEndpoint{ID: "focus", Name: "Login", FilePath: "auth/login.go", StartLine: 10}
	impact := impactResponse{
		Focus: &focus,
		Callers: impactSection{Entries: []impactEntry{
			{Endpoint: neighborEndpoint{ID: "same", Name: "Authorize", FilePath: "auth/mw.go", StartLine: 31}},
			{Endpoint: neighborEndpoint{ID: "other", Name: "AdminPortal", FilePath: "admin/portal.go", StartLine: 7}},
		}},
	}
	affected := collectSimulateAffected(impact)
	byName := map[string]simulateAffected{}
	for _, entry := range affected {
		byName[entry.Endpoint.Name] = entry
	}
	if byName["Login"].CrossModule {
		t.Error("the focus must never be marked cross-module")
	}
	if byName["Authorize"].CrossModule {
		t.Error("a caller in the focus's own module is not cross-module")
	}
	if !byName["AdminPortal"].CrossModule {
		t.Error("a caller in another module must be marked cross-module")
	}
}

// "tests": null and "tests": [] are different claims -- "coverage was never
// computed" versus "no test covers this" -- and only the second one is true
// here. The wire format must not spell them the same way.
func TestSimulateUncoveredSerializesEmptyTestArray(t *testing.T) {
	index := &simulateCoverageIndex{
		symbolsByID:   map[string]sem.SymbolRecord{},
		inboundByID:   map[string][]sem.RelationRecord{},
		testsByStem:   map[string][]sem.SymbolRecord{},
		symbolsByFile: map[string][]sem.SymbolRecord{},
	}
	entry := simulateAffected{
		Endpoint: neighborEndpoint{ID: "x", Name: "Orphan", FilePath: "auth/orphan.go"},
		Tests:    index.coveringTests(neighborEndpoint{ID: "x", Name: "Orphan", FilePath: "auth/orphan.go"}),
	}
	if entry.Tests == nil {
		t.Fatal("coveringTests returned nil; an uncovered symbol must carry an empty slice")
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"tests":[]`) {
		t.Errorf("encoded = %s, want it to contain \"tests\":[]", encoded)
	}
}
