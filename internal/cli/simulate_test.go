package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/entire-graph/internal/sem"
)

// The risk verdict is the one number-shaped thing this command prints, so it is
// the one a reader is most likely to quote out of context. These cases pin the
// boundaries so a later change cannot quietly turn MEDIUM into HIGH.
//
// The reason strings changed with the curveball ("no covering test" -> "no
// RESOLVED covering test"), because the old wording asserted an absence static
// analysis cannot establish. Every THRESHOLD below is unchanged, and
// TestSimulateRiskThresholdsDidNotMove pins that separately, so the rewording
// cannot be used to smuggle a boundary change past this table.
func TestSimulateRiskVerdicts(t *testing.T) {
	cases := []struct {
		name       string
		affected   int
		uncovered  int
		wantRisk   string
		wantReason string
	}{
		{"no blast radius", 0, 0, "NONE", "no affected symbols resolved for this focus"},
		{"fully covered", 8, 0, "LOW", "all 8 affected symbols have a resolved covering test"},
		{"single symbol, covered", 1, 0, "LOW", "all 1 affected symbol has a resolved covering test"},
		{"one gap in many", 20, 1, "MEDIUM", "1 of 20 affected symbols has no resolved covering test"},
		{"majority uncovered below count threshold", 4, 2, "HIGH", "2 of 4 affected symbols have no resolved covering test (majority uncovered)"},
		{"count threshold fires", 40, 5, "HIGH", "5 of 40 affected symbols have no resolved covering test (>= 5 uncovered)"},
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

// ---------------------------------------------------------------------------
// Curveball: the graph is evidence, not an oracle.
//
// Everything below exists because static analysis cannot resolve dynamic
// dispatch, reflection or generated code. The tests come in pairs: one pins
// that weak evidence is labelled weak, and one pins that fully resolved code
// still reads exactly as it did before.
// ---------------------------------------------------------------------------

// The reason strings in TestSimulateRiskVerdicts were reworded for the
// curveball. This pins the NUMBERS independently of the words, so a future
// rewording cannot move a threshold under cover of a wording change.
func TestSimulateRiskThresholdsDidNotMove(t *testing.T) {
	if simulateHighUncoveredCount != 5 {
		t.Errorf("simulateHighUncoveredCount = %d, want 5", simulateHighUncoveredCount)
	}
	if simulateHighUncoveredRatio != 0.5 {
		t.Errorf("simulateHighUncoveredRatio = %v, want 0.5", simulateHighUncoveredRatio)
	}
	cases := []struct {
		affected, uncovered int
		want                string
	}{
		{0, 0, "NONE"}, {8, 0, "LOW"}, {20, 1, "MEDIUM"},
		{4, 2, "HIGH"}, {40, 5, "HIGH"}, {40, 4, "MEDIUM"},
	}
	for _, testCase := range cases {
		if risk, _ := simulateRisk(testCase.affected, testCase.uncovered); risk != testCase.want {
			t.Errorf("simulateRisk(%d, %d) = %q, want %q",
				testCase.affected, testCase.uncovered, risk, testCase.want)
		}
	}
}

// The confirmed/heuristic partition is the whole basis of the evidence tiers,
// and it was read off the provider's own Reason strings. If someone adds a
// resolution value to that switch without a provider reason to back it, this
// test is where the argument has to be had.
func TestSimulateConfirmedResolutionPartition(t *testing.T) {
	for _, resolution := range []string{"exact", "import_resolved", "package"} {
		if !simulateConfirmedResolution(resolution) {
			t.Errorf("resolution %q must count as confirmed", resolution)
		}
	}
	// name_only and type_inferred are the dynamic-dispatch cases; pattern is
	// route/MinHash guessing. None of them traced a call.
	for _, resolution := range []string{"name_only", "type_inferred", "pattern", "import_external"} {
		if simulateConfirmedResolution(resolution) {
			t.Errorf("resolution %q must NOT count as confirmed", resolution)
		}
	}
	// An unknown or absent resolution must degrade toward verification, never
	// toward proof. This is the direction that keeps a provider upgrade from
	// silently minting confidence.
	for _, resolution := range []string{"", "some_future_resolution"} {
		if simulateConfirmedResolution(resolution) {
			t.Errorf("unknown resolution %q must not count as confirmed", resolution)
		}
	}
}

// THE DEFECT THE CURVEBALL EXPOSED.
// A test-file edge the provider resolved only by matching a NAME was labelled
// `edge` -- the tier documented as "the graph proved a test actually runs this
// code". In this repository all 9 TESTS edges are resolved name_only, so this
// was not a hypothetical.
func TestCoveringTestsWithholdsEdgeTierFromNameOnlyRelations(t *testing.T) {
	index := coverageIndexWith(
		sem.SymbolRecord{ID: "svc", Name: "Charge", FilePath: "billing/svc.go", StartLine: 10, Language: "Go"},
		sem.SymbolRecord{ID: "t", Name: "TestCharge", FilePath: "billing/svc_test.go", StartLine: 5, Language: "Go"},
		sem.RelationRecord{FromID: "t", ToID: "svc", Type: "CALLS", Resolution: "name_only"},
	)
	tests := index.coveringTests(neighborEndpoint{ID: "svc", Name: "Charge", FilePath: "billing/svc.go"})
	if len(tests) != 1 {
		t.Fatalf("coveringTests returned %d tests, want 1", len(tests))
	}
	if tests[0].Evidence != simulateEvidenceHeuristic {
		t.Errorf("evidence = %q, want %q: a name_only relation is not proof of execution",
			tests[0].Evidence, simulateEvidenceHeuristic)
	}
	if tests[0].Resolution != "name_only" {
		t.Errorf("resolution = %q, want it carried through verbatim so the tier is auditable",
			tests[0].Resolution)
	}
	if got := simulateCoverageVerdict(tests); got != simulateCoverageHeuristic {
		t.Errorf("verdict = %q, want %q", got, simulateCoverageHeuristic)
	}
}

// An explicit TESTS edge is a HEURISTIC RELATION TYPE in this provider, so it
// cannot reach the confirmed tier even when its resolution looks strong: what
// is heuristic there is the decision to draw the edge at all.
func TestCoveringTestsWithholdsEdgeTierFromHeuristicRelationTypes(t *testing.T) {
	index := coverageIndexWith(
		sem.SymbolRecord{ID: "svc", Name: "Charge", FilePath: "billing/svc.go", StartLine: 10, Language: "Go"},
		sem.SymbolRecord{ID: "t", Name: "TestCharge", FilePath: "billing/svc_test.go", StartLine: 5, Language: "Go"},
		sem.RelationRecord{FromID: "t", ToID: "svc", Type: "TESTS", Resolution: "exact"},
	)
	tests := index.coveringTests(neighborEndpoint{ID: "svc", Name: "Charge", FilePath: "billing/svc.go"})
	if len(tests) != 1 {
		t.Fatalf("coveringTests returned %d tests, want 1", len(tests))
	}
	if tests[0].Evidence != simulateEvidenceHeuristic {
		t.Errorf("evidence = %q, want %q: TESTS is a heuristic relation type here",
			tests[0].Evidence, simulateEvidenceHeuristic)
	}
}

// REQUIREMENT 4, as a test rather than a promise. A resolved call from a test
// file must still be tier `edge` and still verdict `confirmed`. 1292 of the
// 1405 covered symbols in this repository are in this state, and none of their
// output may move.
func TestCoveringTestsKeepsEdgeTierForResolvedRelations(t *testing.T) {
	for _, resolution := range []string{"exact", "import_resolved", "package"} {
		index := coverageIndexWith(
			sem.SymbolRecord{ID: "svc", Name: "Charge", FilePath: "billing/svc.go", StartLine: 10, Language: "Go"},
			sem.SymbolRecord{ID: "t", Name: "TestCharge", FilePath: "billing/svc_test.go", StartLine: 5, Language: "Go"},
			sem.RelationRecord{FromID: "t", ToID: "svc", Type: "CALLS", Resolution: resolution},
		)
		tests := index.coveringTests(neighborEndpoint{ID: "svc", Name: "Charge", FilePath: "billing/svc.go"})
		if len(tests) != 1 {
			t.Fatalf("resolution %q: got %d tests, want 1", resolution, len(tests))
		}
		if tests[0].Evidence != simulateEvidenceEdge {
			t.Errorf("resolution %q: evidence = %q, want %q", resolution, tests[0].Evidence, simulateEvidenceEdge)
		}
		if got := simulateCoverageVerdict(tests); got != simulateCoverageConfirmed {
			t.Errorf("resolution %q: verdict = %q, want %q", resolution, got, simulateCoverageConfirmed)
		}
	}
}

// The three states must stay distinguishable, including the one that matters
// most: an empty test list is "unresolved", never "uncovered", because the
// graph resolving nothing is not the same fact as no test existing.
func TestSimulateCoverageVerdictThreeStates(t *testing.T) {
	if got := simulateCoverageVerdict(nil); got != simulateCoverageUnresolved {
		t.Errorf("no tests -> %q, want %q", got, simulateCoverageUnresolved)
	}
	heuristic := []simulateCoveringTest{{Evidence: simulateEvidenceMirror}, {Evidence: simulateEvidenceHeuristic}}
	if got := simulateCoverageVerdict(heuristic); got != simulateCoverageHeuristic {
		t.Errorf("only weak tests -> %q, want %q", got, simulateCoverageHeuristic)
	}
	// One confirmed edge among weak ones is enough: the strongest evidence wins.
	mixed := []simulateCoveringTest{{Evidence: simulateEvidenceMirror}, {Evidence: simulateEvidenceEdge}}
	if got := simulateCoverageVerdict(mixed); got != simulateCoverageConfirmed {
		t.Errorf("mixed tiers -> %q, want %q", got, simulateCoverageConfirmed)
	}
}

// impact reports Total (every match) and Entries (the capped listing). v1 read
// Entries only, so a capped section silently shrank the risk denominator while
// the header stated it as fact.
func TestSimulateOmittedTotalCountsWhatImpactDidNotList(t *testing.T) {
	impact := impactResponse{
		Callers:       impactSection{Total: 40, Entries: make([]impactEntry, 20)},
		TypeConsumers: impactSection{Total: 3, Entries: make([]impactEntry, 3)},
		DataFlows:     impactSection{Total: 9, Entries: make([]impactEntry, 4)},
		// Callees are not part of the blast radius, so their cap is irrelevant
		// and must not inflate the omitted count.
		Callees: impactSection{Total: 900, Entries: make([]impactEntry, 1)},
	}
	if got := simulateOmittedTotal(impact); got != 25 {
		t.Errorf("simulateOmittedTotal = %d, want 25 (20 callers + 0 + 5 data flows)", got)
	}
	if got := simulateOmittedTotal(impactResponse{
		Callers: impactSection{Total: 2, Entries: make([]impactEntry, 2)},
	}); got != 0 {
		t.Errorf("a complete listing must report 0 omitted, got %d", got)
	}
}

// impact labels an entry it resolved only lexically against a file that cannot
// execute. v1 dropped that label, promoting a doc-mention to a fully affected
// symbol that could then be printed as an unqualified gap.
func TestCollectSimulateAffectedCarriesImpactMentionLabel(t *testing.T) {
	focus := neighborEndpoint{ID: "focus", Name: "Charge", FilePath: "billing/svc.go", StartLine: 10}
	impact := impactResponse{
		Focus: &focus,
		Callers: impactSection{Entries: []impactEntry{
			{Endpoint: neighborEndpoint{ID: "doc", Name: "Charge", FilePath: "docs/billing.md", StartLine: 4},
				Relation: "CALLS", Detail: impactMentionDetail},
			{Endpoint: neighborEndpoint{ID: "mw", Name: "Authorize", FilePath: "billing/mw.go", StartLine: 31},
				Relation: "CALLS"},
		}},
	}
	byID := map[string]simulateAffected{}
	for _, entry := range collectSimulateAffected(impact) {
		byID[entry.Endpoint.ID] = entry
	}
	if !byID["doc"].Mention || byID["doc"].Detail != impactMentionDetail {
		t.Errorf("doc-mention entry = %+v, want Mention set and Detail carried", byID["doc"])
	}
	if byID["mw"].Mention {
		t.Error("a resolved caller must not be marked as a mention")
	}
	if byID["focus"].Mention {
		t.Error("the focus is never a mention")
	}
}

// coverageIndexWith builds a coverage index directly, so a test can state the
// exact relation shape it is about without standing up a repository.
func coverageIndexWith(target, test sem.SymbolRecord, relations ...sem.RelationRecord) *simulateCoverageIndex {
	index := &simulateCoverageIndex{
		symbolsByID:   map[string]sem.SymbolRecord{target.ID: target, test.ID: test},
		inboundByID:   map[string][]sem.RelationRecord{},
		testsByStem:   map[string][]sem.SymbolRecord{},
		symbolsByFile: map[string][]sem.SymbolRecord{},
	}
	for _, relation := range relations {
		index.inboundByID[relation.ToID] = append(index.inboundByID[relation.ToID], relation)
	}
	return index
}

// TestSimulateOnUnresolvableDynamicDispatchFixture is the fixture the curveball
// asked for: a repository whose only path from a test to a production symbol
// runs through dynamic dispatch that static analysis cannot follow.
//
// The fixture is built so the hop is GENUINELY unresolvable, not merely
// unresolved by accident. Gateway has TWO implementations, so the provider's
// "interface method call resolved to the unique implementing method" rule
// cannot fire, and `Charge` is not a globally unique method name either, so the
// name_only fallback cannot fire. The call therefore has no resolvable target.
//
// Ground truth, which the graph cannot see: TestCheckoutWithStripe really does
// execute chargeViaStripe, through Checkout -> Gateway.Charge -> Stripe.Charge.
// A tool that printed "no covering test" here would be stating a falsehood.
// The requirement is that it says what is true instead: no test could be
// RESOLVED, the analysis may be partial, and here is how to check.
func TestSimulateOnUnresolvableDynamicDispatchFixture(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Tests")
	git(t, repo, "config", "user.email", "tests@entire.local")

	write(t, repo, "go.mod", "module fixture\n\ngo 1.21\n")
	write(t, repo, "payments.go", `package app

// Gateway has two implementations on purpose: with more than one, no unique
// implementing method exists and the interface call cannot be resolved.
type Gateway interface {
	Charge(cents int) error
}

type Stripe struct{}

func (s Stripe) Charge(cents int) error { return chargeViaStripe(cents) }

type Adyen struct{}

func (a Adyen) Charge(cents int) error { return chargeViaAdyen(cents) }

func chargeViaStripe(cents int) error { return nil }

func chargeViaAdyen(cents int) error { return nil }

// Checkout dispatches dynamically. This is the hop the graph cannot follow.
func Checkout(g Gateway, cents int) error { return g.Charge(cents) }

// FormatAmount is reached by a direct, statically resolvable call from its
// test. It is here so one run covers both states.
func FormatAmount(cents int) string {
	if cents == 0 {
		return "free"
	}
	return "paid"
}
`)
	write(t, repo, "payments_test.go", `package app

import "testing"

// Really does execute chargeViaStripe. The graph cannot prove it.
func TestCheckoutWithStripe(t *testing.T) {
	if err := Checkout(Stripe{}, 100); err != nil {
		t.Fatal(err)
	}
}

func TestFormatAmount(t *testing.T) {
	if FormatAmount(0) != "free" {
		t.Fatal("bad")
	}
}
`)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "fixture: dynamic dispatch the graph cannot resolve")

	cacheDir := t.TempDir()
	run := func(t *testing.T, symbol, format string) string {
		t.Helper()
		var out bytes.Buffer
		err := Run(t.Context(), Options{
			Version: "0.1.0",
			Env:     EntireEnv{RepoRoot: repo},
			Stdout:  &out,
			Stderr:  &bytes.Buffer{},
		}, []string{"simulate", "--repo", repo, "--symbol", symbol,
			"--cache-dir", cacheDir, "--format", format})
		if err != nil {
			t.Fatalf("simulate %s --format %s: %v", symbol, format, err)
		}
		return out.String()
	}
	decode := func(t *testing.T, payload string) simulateResponse {
		t.Helper()
		var response simulateResponse
		if err := json.Unmarshal([]byte(payload), &response); err != nil {
			t.Fatalf("decode: %v\n%s", err, payload)
		}
		return response
	}

	t.Run("unresolvable dispatch is reported as unresolved, not as absence", func(t *testing.T) {
		response := decode(t, run(t, "chargeViaStripe", "json"))
		focus := focusEntry(t, response)

		if focus.Coverage != simulateCoverageUnresolved {
			t.Fatalf("coverage = %q, want %q. If the provider learned to resolve this "+
				"interface hop, this fixture no longer represents incomplete analysis "+
				"and must be rebuilt with a hop that is still unresolvable.",
				focus.Coverage, simulateCoverageUnresolved)
		}
		if len(focus.Tests) != 0 {
			t.Errorf("tests = %+v, want none resolvable", focus.Tests)
		}
		if !response.AnalysisPartial {
			t.Error("analysis_partial = false; a verdict resting on an unresolved symbol is partial")
		}
		if len(response.PartialReasons) == 0 {
			t.Error("partial_reasons is empty; the flag must always be auditable")
		}
		// Requirement 3: an unproven claim must ship with the way to settle it.
		var verified bool
		for _, item := range response.Verify {
			if item.Symbol == "chargeViaStripe" {
				verified = true
				if !strings.Contains(item.Command, "neighbors") {
					t.Errorf("verify command = %q, want the inbound-relation query", item.Command)
				}
			}
		}
		if !verified {
			t.Errorf("verify has no entry for chargeViaStripe: %+v", response.Verify)
		}
	})

	t.Run("text output never claims the absence is proof", func(t *testing.T) {
		text := run(t, "chargeViaStripe", "text")
		for _, want := range []string{
			"have no test the graph could resolve",
			"not proof no test exists",
			"VERIFY",
			"ANALYSIS MAY BE PARTIAL",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("text is missing %q:\n%s", want, text)
			}
		}
		// The v1 wording asserted an absence. It must not come back.
		if strings.Contains(text, "have no covering test") {
			t.Errorf("text still makes the unqualified claim:\n%s", text)
		}
	})

	t.Run("requirement 4: statically resolved coverage still reads as confirmed", func(t *testing.T) {
		response := decode(t, run(t, "FormatAmount", "json"))
		focus := focusEntry(t, response)
		if focus.Coverage != simulateCoverageConfirmed {
			t.Fatalf("coverage = %q, want %q; a direct resolved call from a test is "+
				"exactly the case that must not change", focus.Coverage, simulateCoverageConfirmed)
		}
		if len(focus.Tests) == 0 {
			t.Fatal("no tests resolved for a directly tested function")
		}
		if focus.Tests[0].Evidence != simulateEvidenceEdge {
			t.Errorf("evidence = %q, want %q", focus.Tests[0].Evidence, simulateEvidenceEdge)
		}
		if !simulateConfirmedResolution(focus.Tests[0].Resolution) {
			t.Errorf("resolution = %q, want a confirmed one", focus.Tests[0].Resolution)
		}
		if response.UncoveredTotal != 0 {
			t.Errorf("uncovered_total = %d, want 0 for a fully resolved focus", response.UncoveredTotal)
		}
		if response.Risk != "LOW" {
			t.Errorf("risk = %q, want LOW", response.Risk)
		}
		// Nothing weak in this answer, so nothing may be flagged as weak.
		for _, item := range response.Verify {
			if item.Symbol == "FormatAmount" {
				t.Errorf("a confirmed symbol must not appear in VERIFY: %+v", item)
			}
		}
	})
}

// focusEntry returns the affected entry for the symbol under change.
func focusEntry(t *testing.T, response simulateResponse) simulateAffected {
	t.Helper()
	for _, entry := range response.Affected {
		if entry.Focus {
			return entry
		}
	}
	t.Fatalf("no focus entry in affected set: %+v", response.Affected)
	return simulateAffected{}
}
