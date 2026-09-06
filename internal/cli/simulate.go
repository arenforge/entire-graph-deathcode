package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-graph/internal/sem"
	"github.com/entireio/entire-graph/internal/termsafe"
)

// Change simulation
// =================
//
// `impact` answers "what breaks if I change this?". It does not answer the
// question a developer actually stops on before editing: "will anything CATCH
// it if I get this wrong?" The graph already knows — it resolves TESTS edges,
// and `search` uses them to name one covering test for one symbol. Nothing
// composes those two: impact.go has no TESTS handling at all, so a blast radius
// of fourteen symbols arrives with no indication of which nine are unguarded.
//
// simulate is that composition. It takes impact's affected set and, for every
// symbol in it, asks whether a test reaches it, then reports the symbols where
// the answer is no. That list — not the blast radius, which impact already
// prints — is the output this command exists to produce.
//
// Every claim carries file:line and the evidence tier it came from, because a
// coverage verdict a reviewer cannot check is worse than none: it invites trust
// it has not earned. Nothing here is scored on an invented scale, and no
// section is emitted from a guess. When the graph cannot resolve coverage for a
// symbol, that symbol is UNCOVERED and the tier says why.

const (
	// simulateFormatVersion is bumped when the wire shape changes
	// incompatibly, matching how the other query commands version themselves.
	simulateFormatVersion = 1

	// simulateHighUncoveredRatio / simulateHighUncoveredCount are the two ways
	// a change earns HIGH. A ratio alone mislabels a two-symbol change with one
	// gap; a count alone mislabels a ninety-symbol change with six. Either
	// condition is sufficient, and the reason string always states which fired,
	// so the verdict is reproducible by hand from the printed counts.
	simulateHighUncoveredRatio = 0.5
	simulateHighUncoveredCount = 5
)

// Coverage evidence tiers, strongest first. These mirror the tiers
// search_covertest.go established, deliberately: a reader who has learned what
// `edge` means in a search result should not have to learn a second vocabulary
// here. `edge` is a graph-resolved inbound CALLS/TESTS relation from a symbol
// in a test file. `mirror` is a test living in the anchor's mirror test file
// that also names the anchor — weaker, and labelled so, because file adjacency
// alone proves nothing about what the test executes.
const (
	simulateEvidenceEdge   = "edge"
	simulateEvidenceMirror = "mirror"
)

// simulateCoveringTest is one test that reaches an affected symbol.
type simulateCoveringTest struct {
	Endpoint neighborEndpoint `json:"endpoint"`
	Relation string           `json:"relation,omitempty"`
	Evidence string           `json:"evidence"`
}

// simulateAffected is one symbol in the blast radius, with the tests that
// reach it. An empty Tests slice is the finding, not missing data.
type simulateAffected struct {
	Endpoint neighborEndpoint `json:"endpoint"`
	Relation string           `json:"relation,omitempty"`
	Depth    int              `json:"depth,omitempty"`
	Focus    bool             `json:"focus,omitempty"`
	Module   string           `json:"module,omitempty"`
	// CrossModule marks an affected symbol that lives outside the focus's own
	// module. These are the callers a signature or behaviour change can break
	// for someone who does not read this diff, which is why they are reported
	// separately rather than folded into the affected count.
	CrossModule bool                   `json:"cross_module,omitempty"`
	Tests       []simulateCoveringTest `json:"tests"`
}

type simulateResponse struct {
	FormatVersion int    `json:"format_version"`
	RepoRoot      string `json:"repo_root"`
	Commit        string `json:"commit,omitempty"`
	Profile       string `json:"profile"`
	Query         string `json:"query"`

	// Focus resolution is delegated to impact's resolver, so an ambiguous or
	// misspelled name is answered here exactly as it is there.
	DisambiguationRequired bool               `json:"disambiguation_required"`
	Definitions            []neighborEndpoint `json:"definitions,omitempty"`
	Focus                  *neighborEndpoint  `json:"focus,omitempty"`

	Affected       []simulateAffected `json:"affected"`
	AffectedTotal  int                `json:"affected_total"`
	CoveredTotal   int                `json:"covered_total"`
	UncoveredTotal int                `json:"uncovered_total"`
	// CrossModuleTotal counts affected symbols outside the focus's module;
	// CrossModuleUncoveredTotal is the subset of those with no covering test.
	// The second number is the worst case this command can report: a caller in
	// another module that no test guards.
	CrossModuleTotal          int      `json:"cross_module_total"`
	CrossModuleUncoveredTotal int      `json:"cross_module_uncovered_total"`
	Modules                   []string `json:"modules"`

	Risk       string `json:"risk"`
	RiskReason string `json:"risk_reason"`

	// TestsHeuristic records that TESTS is a heuristic relation type in this
	// provider (see HeuristicRelationTypes in sem/provider.go). A consumer that
	// gates a merge on this output is entitled to know that before it does.
	TestsHeuristic bool `json:"tests_heuristic"`

	IndexCacheHit  bool  `json:"index_cache_hit"`
	IndexLatencyMS int64 `json:"index_latency_ms"`
	QueryLatencyMS int64 `json:"query_latency_ms"`
	TotalLatencyMS int64 `json:"total_latency_ms"`

	Warnings        []sem.ProviderWarning  `json:"warnings,omitempty"`
	PartialFailures []sem.PartialFailure   `json:"partial_failures"`
	Completeness    sem.CompletenessReport `json:"completeness"`
}

func runSimulate(ctx context.Context, opts Options, args []string) error {
	flags, err := parseSimulateFlags(args)
	if err != nil {
		return err
	}
	repo, err := resolveRepo(ctx, opts.Env, flags.Repo)
	if err != nil {
		return err
	}
	profile, err := parseProfile(flags.Profile)
	if err != nil {
		return err
	}
	if flags.Format != "text" && flags.Format != "json" {
		return fmt.Errorf("simulate --format must be text or json, got %q", flags.Format)
	}
	cacheDir := resolveCacheDir(flags.CacheDir, opts.Env.PluginDataDir)
	totalStarted := time.Now()
	indexStarted := totalStarted
	snapshot, cacheHit, err := sem.LoadOrBuildProviderSnapshot(ctx, repo, opts.Version, sem.ProviderSnapshotOptions{
		NoNetwork:    true,
		Worktree:     flags.Worktree,
		IgnoreFiles:  flags.IgnoreFile,
		IncludeFiles: flags.IncludeFile,
		Profile:      profile,
	}, cacheDir, flags.DisableCache)
	if err != nil {
		return err
	}
	readSource, closeSource := openSnapshotLineReaderOrDegrade(ctx, snapshot, flags.Worktree, opts.Stderr)
	if closeSource != nil {
		defer closeSource()
	}
	indexLatency := time.Since(indexStarted)

	queryStarted := time.Now()
	// The blast radius is impact's answer, unchanged. Recomputing it here would
	// let the two commands disagree about what "affected" means, and a reviewer
	// who ran both would have no way to tell which one to believe.
	impact := buildImpactResponseFromReader(snapshot, flags, readSource)
	response := buildSimulateResponse(snapshot, impact, flags.Limit)
	queryLatency := time.Since(queryStarted)

	response.IndexCacheHit = cacheHit
	response.IndexLatencyMS = indexLatency.Milliseconds()
	response.QueryLatencyMS = queryLatency.Milliseconds()
	response.TotalLatencyMS = time.Since(totalStarted).Milliseconds()

	if flags.Format == "json" {
		encoder := json.NewEncoder(termsafe.NewJSONWriter(opts.Stdout))
		encoder.SetEscapeHTML(false)
		return encoder.Encode(response)
	}
	writeSimulateText(opts.Stdout, response)
	return nil
}

// parseSimulateFlags delegates to impact's parser. simulate resolves the same
// focus from the same snapshot with the same cache and profile flags, so a
// second copy of that loop would only create somewhere for the two to drift.
// The error text is re-labelled because a user who typed `simulate` should not
// be told about a command they did not run.
func parseSimulateFlags(args []string) (impactFlags, error) {
	flags, err := parseImpactFlags(args)
	if err != nil {
		return flags, errors.New(strings.Replace(err.Error(), "impact", "simulate", 1))
	}
	return flags, nil
}

// buildSimulateResponse is a pure projection of a snapshot and an already-built
// impact response: no re-parsing, no second pass over source, no network. Given
// the same two inputs it returns the same bytes, which is what makes the
// verdict reproducible in a test.
func buildSimulateResponse(snapshot sem.ProviderSnapshot, impact impactResponse, limit int) simulateResponse {
	response := simulateResponse{
		FormatVersion:          simulateFormatVersion,
		RepoRoot:               impact.RepoRoot,
		Commit:                 impact.Commit,
		Profile:                impact.Profile,
		Query:                  impact.Query,
		DisambiguationRequired: impact.DisambiguationRequired,
		Definitions:            impact.Definitions,
		Focus:                  impact.Focus,
		TestsHeuristic:         true,
		Warnings:               impact.Warnings,
		PartialFailures:        impact.PartialFailures,
		Completeness:           impact.Completeness,
		Affected:               []simulateAffected{},
		Modules:                []string{},
	}
	// An ambiguous name has no single blast radius, so there is nothing to
	// simulate. Returning impact's definition list unchanged lets the caller
	// disambiguate with the same flags it would have used there.
	if impact.DisambiguationRequired || impact.Focus == nil {
		return response
	}

	index := newSimulateCoverageIndex(snapshot)
	affected := collectSimulateAffected(impact)

	for _, entry := range affected {
		entry.Tests = index.coveringTests(entry.Endpoint)
		if len(entry.Tests) == 0 {
			response.UncoveredTotal++
		} else {
			response.CoveredTotal++
		}
		if entry.CrossModule {
			response.CrossModuleTotal++
			if len(entry.Tests) == 0 {
				response.CrossModuleUncoveredTotal++
			}
		}
		response.Affected = append(response.Affected, entry)
	}
	response.AffectedTotal = len(response.Affected)
	response.Modules = simulateModules(response.Affected)

	// The cap is applied after the counts, so a truncated listing still reports
	// the true totals it was computed from. Uncovered entries survive
	// truncation first: they are the reason to run this command.
	if limit > 0 && len(response.Affected) > limit {
		response.Affected = capSimulateAffected(response.Affected, limit)
	}
	response.Risk, response.RiskReason = simulateRisk(response.AffectedTotal, response.UncoveredTotal)
	return response
}

// collectSimulateAffected flattens impact's sections into one deduplicated set
// of production symbols a change to the focus can reach.
//
// Callers, type consumers and data flows are in. Callees are NOT: changing a
// function does not break the functions it calls. Co-changes and siblings are
// not either — both are correlations, and a coverage gap reported against a
// symbol that a change cannot actually break is a false alarm, which is the one
// failure mode that would make a reviewer stop reading this output.
//
// The focus itself leads the list. It is the symbol being edited, so whether a
// test guards IT is the first thing a reader wants; omitting it would report
// coverage for everything except the thing under change.
func collectSimulateAffected(impact impactResponse) []simulateAffected {
	seen := map[string]bool{}
	var out []simulateAffected

	focusModule := ""
	if impact.Focus != nil {
		focusModule = simulateModuleOf(impact.Focus.FilePath)
	}

	add := func(endpoint neighborEndpoint, relation string, depth int, focus bool) {
		// External symbols have no body in this repository, so no test here can
		// cover them and reporting them as gaps would be noise.
		if endpoint.External || endpoint.ID == "" {
			return
		}
		// A test that would be reported as uncovered production code is a
		// category error: tests are the instrument, not the subject.
		if isConventionalTestPath(endpoint.FilePath) {
			return
		}
		if seen[endpoint.ID] {
			return
		}
		seen[endpoint.ID] = true
		module := simulateModuleOf(endpoint.FilePath)
		out = append(out, simulateAffected{
			Endpoint:    endpoint,
			Relation:    relation,
			Depth:       depth,
			Focus:       focus,
			Module:      module,
			CrossModule: !focus && module != focusModule,
		})
	}

	if impact.Focus != nil {
		add(*impact.Focus, "", 0, true)
	}
	for _, section := range []impactSection{impact.Callers, impact.TypeConsumers, impact.DataFlows} {
		for _, entry := range section.Entries {
			add(entry.Endpoint, entry.Relation, entry.Depth, false)
		}
	}
	return out
}

// simulateCoverageIndex answers "does a test reach this symbol?" from snapshot
// records alone. It is built once per run: the inbound-relation walk is O(all
// relations), and doing it per affected symbol would make a fourteen-symbol
// blast radius fourteen passes over the graph.
type simulateCoverageIndex struct {
	symbolsByID   map[string]sem.SymbolRecord
	inboundByID   map[string][]sem.RelationRecord
	testsByStem   map[string][]sem.SymbolRecord
	symbolsByFile map[string][]sem.SymbolRecord
}

func newSimulateCoverageIndex(snapshot sem.ProviderSnapshot) *simulateCoverageIndex {
	index := &simulateCoverageIndex{
		symbolsByID:   make(map[string]sem.SymbolRecord, len(snapshot.Symbols)),
		inboundByID:   map[string][]sem.RelationRecord{},
		testsByStem:   map[string][]sem.SymbolRecord{},
		symbolsByFile: map[string][]sem.SymbolRecord{},
	}
	for _, symbol := range snapshot.Symbols {
		index.symbolsByID[symbol.ID] = symbol
		index.symbolsByFile[symbol.FilePath] = append(index.symbolsByFile[symbol.FilePath], symbol)
		if isConventionalTestPath(symbol.FilePath) {
			stem := simulateFileStem(symbol.FilePath)
			index.testsByStem[stem] = append(index.testsByStem[stem], symbol)
		}
	}
	for _, relation := range snapshot.Relations {
		if !simulateCoverageRelation(relation.Type) {
			continue
		}
		index.inboundByID[relation.ToID] = append(index.inboundByID[relation.ToID], relation)
	}
	return index
}

// simulateCoverageRelation is the family of edges that mean "this test
// executes that code". TESTS is the explicit one; the call family is included
// because a test function that calls a symbol directly exercises it whether or
// not the provider also emitted a TESTS edge.
func simulateCoverageRelation(relationType string) bool {
	return relationType == "TESTS" || impactCallRelation(relationType)
}

func (index *simulateCoverageIndex) coveringTests(endpoint neighborEndpoint) []simulateCoveringTest {
	// Non-nil from the start: an uncovered symbol must serialize as "tests": []
	// and not "tests": null. A consumer gating a merge on this output should be
	// able to read len(tests) without first distinguishing "no tests found"
	// from "coverage was never computed" -- and those two are NOT the same
	// claim, so the wire format must not spell them identically.
	tests := []simulateCoveringTest{}
	seen := map[string]bool{}

	// Tier 1, edge: the graph resolved that a symbol in a test file reaches
	// this one. This is the only tier that is evidence of execution.
	for _, relation := range index.inboundByID[endpoint.ID] {
		source, ok := index.symbolsByID[relation.FromID]
		if !ok || !isConventionalTestPath(source.FilePath) || seen[source.ID] {
			continue
		}
		seen[source.ID] = true
		tests = append(tests, simulateCoveringTest{
			Endpoint: simulateEndpointOf(source),
			Relation: relation.Type,
			Evidence: simulateEvidenceEdge,
		})
	}
	if len(tests) > 0 {
		sortSimulateTests(tests)
		return tests
	}

	// Tier 2, mirror: a test in the anchor's mirror test file that also NAMES
	// the anchor. Adjacency alone is not enough to pick a test method out of a
	// file, so the name requirement is not optional — without it this tier
	// would credit every test in foo_test.go with covering every symbol in
	// foo.go, which is exactly the kind of unearned coverage claim this
	// command exists to expose.
	stem := simulateFileStem(endpoint.FilePath)
	if stem == "" || endpoint.Name == "" {
		return tests
	}
	for _, candidate := range index.testsByStem[stem] {
		if candidate.FilePath == endpoint.FilePath || seen[candidate.ID] {
			continue
		}
		if !strings.Contains(candidate.Name, endpoint.Name) {
			continue
		}
		seen[candidate.ID] = true
		tests = append(tests, simulateCoveringTest{
			Endpoint: simulateEndpointOf(candidate),
			Evidence: simulateEvidenceMirror,
		})
	}
	sortSimulateTests(tests)
	return tests
}

// simulateFileStem strips the directory and every extension, plus the
// conventional test affixes, so `internal/cli/impact_test.go` and
// `internal/cli/impact.go` reduce to the same `impact`. Comparing stems this
// way avoids a per-language path table, which is the same trade
// search_covertest.go makes.
func simulateFileStem(filePath string) string {
	if filePath == "" {
		return ""
	}
	base := path.Base(filePath)
	if cut := strings.IndexByte(base, '.'); cut > 0 {
		base = base[:cut]
	}
	lower := strings.ToLower(base)
	for _, affix := range []string{"_test", "test_", ".test", "-test", "_spec", "spec_"} {
		trimmed := strings.TrimSuffix(lower, affix)
		trimmed = strings.TrimPrefix(trimmed, affix)
		if trimmed != lower {
			lower = trimmed
		}
	}
	return strings.Trim(lower, "_-.")
}

// simulateModuleOf names the module a symbol lives in. The directory is used
// deliberately: it is the one grouping that is true in every language this
// provider parses, where a package or namespace declaration is not.
func simulateModuleOf(filePath string) string {
	if filePath == "" {
		return ""
	}
	dir := path.Dir(filePath)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

func simulateModules(affected []simulateAffected) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, entry := range affected {
		if entry.Module == "" || seen[entry.Module] {
			continue
		}
		seen[entry.Module] = true
		out = append(out, entry.Module)
	}
	sort.Strings(out)
	return out
}

// simulateRisk states a verdict a reader can recompute from the two printed
// counts. It is deliberately three words and not a number: this provider has
// no calibrated basis for a decimal score, and inventing one in a repository
// whose own README retracts over-precise figures would be indefensible.
func simulateRisk(affectedTotal, uncoveredTotal int) (string, string) {
	if affectedTotal == 0 {
		return "NONE", "no affected symbols resolved for this focus"
	}
	if uncoveredTotal == 0 {
		return "LOW", fmt.Sprintf("all %d affected symbols have a covering test", affectedTotal)
	}
	ratio := float64(uncoveredTotal) / float64(affectedTotal)
	reason := fmt.Sprintf("%d of %d affected symbols have no covering test", uncoveredTotal, affectedTotal)
	if uncoveredTotal >= simulateHighUncoveredCount {
		return "HIGH", reason + fmt.Sprintf(" (>= %d uncovered)", simulateHighUncoveredCount)
	}
	if ratio >= simulateHighUncoveredRatio {
		return "HIGH", reason + " (majority uncovered)"
	}
	return "MEDIUM", reason
}

// capSimulateAffected truncates the listing uncovered-first. The totals are
// computed before this runs, so truncation changes what is shown and never
// what is counted.
func capSimulateAffected(affected []simulateAffected, limit int) []simulateAffected {
	kept := make([]simulateAffected, 0, limit)
	for _, entry := range affected {
		if len(entry.Tests) == 0 && len(kept) < limit {
			kept = append(kept, entry)
		}
	}
	for _, entry := range affected {
		if len(entry.Tests) > 0 && len(kept) < limit {
			kept = append(kept, entry)
		}
	}
	return kept
}

func simulateEndpointOf(symbol sem.SymbolRecord) neighborEndpoint {
	return neighborEndpoint{
		ID:            symbol.ID,
		Name:          symbol.Name,
		QualifiedName: symbol.QualifiedName,
		Kind:          symbol.Kind,
		FilePath:      symbol.FilePath,
		StartLine:     symbol.StartLine,
		EndLine:       symbol.EndLine,
		Language:      symbol.Language,
	}
}

// sortSimulateTests orders by location so the same snapshot always renders the
// same bytes. Map iteration feeds this list, so without it the output would be
// unstable and untestable.
func sortSimulateTests(tests []simulateCoveringTest) {
	sort.Slice(tests, func(left, right int) bool {
		if tests[left].Endpoint.FilePath != tests[right].Endpoint.FilePath {
			return tests[left].Endpoint.FilePath < tests[right].Endpoint.FilePath
		}
		if tests[left].Endpoint.StartLine != tests[right].Endpoint.StartLine {
			return tests[left].Endpoint.StartLine < tests[right].Endpoint.StartLine
		}
		return tests[left].Endpoint.Name < tests[right].Endpoint.Name
	})
}

func simulateLocation(endpoint neighborEndpoint) string {
	if endpoint.FilePath == "" {
		return endpoint.Name
	}
	if endpoint.StartLine > 0 {
		return fmt.Sprintf("%s:%d", endpoint.FilePath, endpoint.StartLine)
	}
	return endpoint.FilePath
}

// simulatePluralSymbols keeps the count line readable at one. A header that
// reads "1 affected symbols" undercuts a report whose whole claim is care.
func simulatePluralSymbols(count int) string {
	if count == 1 {
		return "symbol is"
	}
	return "symbols are"
}

// simulateFocusModuleLabel names the module the change originates in, so the
// BREAKING header states what "outside" is measured against instead of leaving
// the reader to guess.
func simulateFocusModuleLabel(response simulateResponse) string {
	if response.Focus == nil {
		return "the focus module"
	}
	if module := simulateModuleOf(response.Focus.FilePath); module != "" {
		return module
	}
	return "the repository root"
}

func writeSimulateText(out io.Writer, response simulateResponse) {
	writer := termsafe.NewWriter(out)

	if response.DisambiguationRequired {
		fmt.Fprintf(writer, "Change simulation: %s\n\n", response.Query)
		fmt.Fprintf(writer, "AMBIGUOUS  %d definitions match; rerun with --file/--line/--kind\n", len(response.Definitions))
		for _, definition := range response.Definitions {
			fmt.Fprintf(writer, "  %-28s %s\n", definition.Name, simulateLocation(definition))
		}
		return
	}

	focus := response.Query
	if response.Focus != nil {
		focus = fmt.Sprintf("%s  %s", response.Focus.Name, simulateLocation(*response.Focus))
	}
	fmt.Fprintf(writer, "Change simulation: %s\n", focus)
	if response.IndexCacheHit {
		fmt.Fprintln(writer, "Index: cache-hit")
	} else {
		fmt.Fprintln(writer, "Index: cache-miss")
	}

	fmt.Fprintf(writer, "\nAFFECTED   %d symbols across %d module(s)\n",
		response.AffectedTotal, len(response.Modules))
	for _, module := range response.Modules {
		fmt.Fprintf(writer, "  %s\n", module)
	}

	var uncovered []simulateAffected
	covered := 0
	fmt.Fprintf(writer, "\nTESTS      %d of %d affected symbols have a covering test\n",
		response.CoveredTotal, response.AffectedTotal)
	for _, entry := range response.Affected {
		if len(entry.Tests) == 0 {
			uncovered = append(uncovered, entry)
			continue
		}
		covered++
		test := entry.Tests[0]
		extra := ""
		if len(entry.Tests) > 1 {
			extra = fmt.Sprintf(" (+%d more)", len(entry.Tests)-1)
		}
		fmt.Fprintf(writer, "  %-30s %-8s %s -> %s%s\n",
			simulateLocation(test.Endpoint), test.Evidence, test.Endpoint.Name, entry.Endpoint.Name, extra)
	}
	if covered == 0 {
		fmt.Fprintln(writer, "  (none)")
	}

	// BREAKING names the affected symbols outside the focus's own module. A
	// developer reading their own diff sees the same-module callers for free;
	// these are the ones they will not notice. Cross-module AND uncovered is
	// the worst case, so it is marked inline rather than left to be inferred.
	fmt.Fprintf(writer, "\nBREAKING   %d affected %s outside %s (%d uncovered)\n",
		response.CrossModuleTotal, simulatePluralSymbols(response.CrossModuleTotal),
		simulateFocusModuleLabel(response), response.CrossModuleUncoveredTotal)
	printedBreaking := false
	for _, entry := range response.Affected {
		if !entry.CrossModule {
			continue
		}
		printedBreaking = true
		marker := ""
		if len(entry.Tests) == 0 {
			marker = "  ** no covering test **"
		}
		fmt.Fprintf(writer, "  %-30s %s%s\n", entry.Endpoint.Name, simulateLocation(entry.Endpoint), marker)
	}
	if !printedBreaking {
		fmt.Fprintln(writer, "  (none - the blast radius stays inside one module)")
	}

	// UNCOVERED is the finding. It is printed last so it is what remains on
	// screen, and it names a location per line because the only useful next
	// action is to open one and write a test.
	fmt.Fprintf(writer, "\nUNCOVERED  %d affected symbols have no covering test\n", response.UncoveredTotal)
	for _, entry := range uncovered {
		marker := ""
		if entry.Focus {
			marker = "  <- the symbol you are changing"
		}
		fmt.Fprintf(writer, "  %-30s %s%s\n", entry.Endpoint.Name, simulateLocation(entry.Endpoint), marker)
	}
	if len(uncovered) == 0 {
		fmt.Fprintln(writer, "  (none)")
	}

	fmt.Fprintf(writer, "\nRISK  %s - %s\n", response.Risk, response.RiskReason)
	if response.TestsHeuristic {
		fmt.Fprintln(writer, "Note: TESTS is a heuristic relation in this provider. Open the")
		fmt.Fprintln(writer, "cited lines before gating a merge on this verdict.")
	}
}
