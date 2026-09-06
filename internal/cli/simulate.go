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
// section is emitted from a guess.
//
// THE GRAPH IS EVIDENCE, NOT AN ORACLE.
// Static analysis cannot resolve dynamic dispatch, reflection, or generated
// code. That leaves two states this command must never collapse into a fact:
//
//   - An edge the provider drew by matching a NAME rather than by resolving a
//     receiver type. Present, but not proof that the test executes the symbol.
//   - No edge at all, which means EITHER no test reaches the symbol OR the
//     resolver could not follow the path that does.
//
// So coverage is reported in three states -- confirmed / heuristic /
// unresolved (simulateCoverage*) -- every test line carries the tier its edge
// was resolved at, and a symbol with no resolved test is reported as "no test
// the graph could resolve", never as "no test exists". Anything that is not
// confirmed gets a printed command that settles it from source or a test run.

const (
	// simulateFormatVersion is bumped when the wire shape changes
	// incompatibly, matching how the other query commands version themselves.
	//
	// v2 added the coverage verdict, the partial-analysis fields, and a THIRD
	// possible value of `evidence` ("edge-heuristic"). A consumer that switched
	// exhaustively on the v1 tiers would silently mis-read the new one, so this
	// is a compatibility break and is versioned as one.
	simulateFormatVersion = 2

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
	// simulateEvidenceEdge is a graph-resolved inbound relation whose RESOLUTION
	// the provider itself describes as resolved (see simulateConfirmedResolution).
	// This is the only tier that is evidence the test executes the code.
	simulateEvidenceEdge = "edge"
	// simulateEvidenceHeuristic is a real inbound relation from a test file that
	// the provider resolved by name, by pattern, or by inferring a receiver type.
	// It is the dynamic-dispatch case: the edge exists because exactly one
	// workspace symbol bore that name, not because the call was traced. Add a
	// second implementation of the interface and the edge becomes wrong without
	// anything in the graph changing, so it cannot share a label with `edge`.
	simulateEvidenceHeuristic = "edge-heuristic"
	// simulateEvidenceMirror is a test living in the anchor's mirror test file
	// that also names the anchor -- weaker still, and labelled so, because file
	// adjacency alone proves nothing about what the test executes.
	simulateEvidenceMirror = "mirror"
)

// Coverage verdicts. Three states, because the graph returns three: a resolved
// test edge, an edge that is only a name match, and nothing at all -- and the
// third does NOT mean "no test exists", it means "no test was resolved".
const (
	simulateCoverageConfirmed  = "confirmed"
	simulateCoverageHeuristic  = "heuristic"
	simulateCoverageUnresolved = "unresolved"
)

// simulateConfirmedResolution reports whether the provider resolved a relation
// by following the program's own structure, rather than by matching a name.
//
// The partition is READ OFF THE PROVIDER, not chosen here. Each value below is
// paired with the Reason string the provider emits alongside it:
//
//	exact            "direct call expression resolved to ..."
//	import_resolved  "resolved through import qualifier to defining module"
//	package          "direct call expression resolved to same-package symbol"
//	                 "... resolved to same-directory symbol"
//
// and the values deliberately excluded:
//
//	name_only        "matched globally unique symbol name"
//	                 "method call matched globally unique method name"
//	type_inferred    receiver type inferred rather than declared
//	pattern          "route-like string literal found inside handler symbol"
//	                 "near-duplicate symbol body (MinHash estimate)"
//
// An unknown or empty resolution is NOT confirmed. A relation shape this
// function has never seen must degrade toward asking for verification, never
// toward claiming proof -- the same direction completenessScopeOrAll fails in.
func simulateConfirmedResolution(resolution string) bool {
	switch resolution {
	case "exact", "import_resolved", "package":
		return true
	default:
		return false
	}
}

// simulateHeuristicRelationType lists the relation types the provider itself
// reports as heuristic (CapabilityReport.HeuristicRelationTypes). TESTS is the
// one that reaches this command, and in this repository all 9 TESTS edges are
// resolved name_only -- so the type alone is enough to withhold `edge`.
func simulateHeuristicRelationType(relationType string) bool {
	return relationType == "TESTS"
}

// simulateEvidenceTier labels one inbound relation. Order matters: a heuristic
// RELATION TYPE cannot be redeemed by a confirmed resolution, because what is
// heuristic there is the decision to draw the edge at all.
func simulateEvidenceTier(relation sem.RelationRecord) string {
	if simulateHeuristicRelationType(relation.Type) {
		return simulateEvidenceHeuristic
	}
	if simulateConfirmedResolution(relation.Resolution) {
		return simulateEvidenceEdge
	}
	return simulateEvidenceHeuristic
}

// simulateCoveringTest is one test that reaches an affected symbol.
type simulateCoveringTest struct {
	Endpoint neighborEndpoint `json:"endpoint"`
	Relation string           `json:"relation,omitempty"`
	Evidence string           `json:"evidence"`
	// Resolution is the provider's own word for HOW the edge was resolved,
	// carried through verbatim so a reader can audit the tier above it instead
	// of taking it on trust. Empty for the mirror tier, which has no edge.
	Resolution string `json:"resolution,omitempty"`
}

// simulateCoverageVerdict collapses a symbol's test list into one of three
// states. Confirmed requires at least one `edge`-tier test; anything weaker is
// heuristic; an empty list is unresolved and is never called "uncovered".
func simulateCoverageVerdict(tests []simulateCoveringTest) string {
	if len(tests) == 0 {
		return simulateCoverageUnresolved
	}
	for _, test := range tests {
		if test.Evidence == simulateEvidenceEdge {
			return simulateCoverageConfirmed
		}
	}
	return simulateCoverageHeuristic
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
	// Coverage is the three-state verdict for this symbol: confirmed,
	// heuristic, or unresolved. An empty Tests slice means the graph resolved
	// no test, which is NOT the same claim as "no test exists" -- so the
	// verdict word is "unresolved" and the wire format says so too.
	Coverage string `json:"coverage"`
	// Detail carries impact's own label for an entry it could not fully
	// resolve (impactMentionDetail, "doc-mention, name_only"). Dropping it
	// promoted a lexical mention in a prose file to a fully affected symbol.
	Detail string `json:"detail,omitempty"`
	// Mention marks that Detail is impact's doc-mention label: the endpoint
	// cannot execute and the match was lexical, so it is reported apart from
	// the symbols a change can really break.
	Mention bool `json:"mention,omitempty"`
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
	// ConfirmedTotal / HeuristicTotal split CoveredTotal by evidence strength.
	// CoveredTotal keeps its v1 meaning (at least one test of any tier) so the
	// risk arithmetic stays the one a reader can recompute from the header;
	// these two say how much of it would survive if the weak edges did not.
	ConfirmedTotal int `json:"confirmed_total"`
	HeuristicTotal int `json:"heuristic_total"`
	// MentionTotal counts affected entries that are impact doc-mentions.
	MentionTotal int `json:"mention_total"`
	// AffectedOmittedTotal is how many matches impact found but did not list,
	// summed over the sections this command reads. Non-zero means the affected
	// set below is a SUBSET and every total derived from it is a lower bound.
	AffectedOmittedTotal int  `json:"affected_omitted_total"`
	AffectedTruncated    bool `json:"affected_truncated"`
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

	// AnalysisPartial is the single field a gate should read before trusting
	// anything else here: true means at least one input to this verdict was
	// incomplete. PartialReasons names each one, so the flag is auditable and
	// never has to be taken on faith.
	AnalysisPartial bool     `json:"analysis_partial"`
	PartialReasons  []string `json:"partial_reasons"`
	// Verify is the fallback path: one entry per claim that is not confirmed
	// structural evidence, each with a command that settles it. An agent can
	// act on this list without parsing the text output.
	Verify []simulateVerification `json:"verify"`

	IndexCacheHit  bool  `json:"index_cache_hit"`
	IndexLatencyMS int64 `json:"index_latency_ms"`
	QueryLatencyMS int64 `json:"query_latency_ms"`
	TotalLatencyMS int64 `json:"total_latency_ms"`

	Warnings        []sem.ProviderWarning  `json:"warnings,omitempty"`
	PartialFailures []sem.PartialFailure   `json:"partial_failures"`
	Stats           sem.ProviderStats      `json:"stats"`
	Completeness    sem.CompletenessReport `json:"completeness"`
	// CompletenessScope is the query-relative reading of the diagnostics above.
	// v1 carried the raw diagnostics and never rendered them, so a snapshot the
	// provider itself called "degraded" produced an unqualified RISK line. This
	// is the same scope impact builds, rendered by the same writer.
	CompletenessScope completenessScope `json:"completeness_scope"`
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
		Stats:                  impact.Stats,
		Completeness:           impact.Completeness,
		// impact builds this scope and renders it on every run; v1 accepted the
		// raw diagnostics and dropped the scope, so simulate printed no
		// completeness line at all on the very same snapshot.
		CompletenessScope: impact.CompletenessScope,
		Affected:          []simulateAffected{},
		Modules:           []string{},
		PartialReasons:    []string{},
		Verify:            []simulateVerification{},
	}
	// An ambiguous name has no single blast radius, so there is nothing to
	// simulate. Returning impact's definition list unchanged lets the caller
	// disambiguate with the same flags it would have used there.
	if impact.DisambiguationRequired || impact.Focus == nil {
		response.AnalysisPartial, response.PartialReasons = simulatePartiality(response)
		return response
	}

	index := newSimulateCoverageIndex(snapshot)
	affected := collectSimulateAffected(impact)

	response.AffectedOmittedTotal = simulateOmittedTotal(impact)
	response.AffectedTruncated = response.AffectedOmittedTotal > 0

	for _, entry := range affected {
		entry.Tests = index.coveringTests(entry.Endpoint)
		entry.Coverage = simulateCoverageVerdict(entry.Tests)
		switch entry.Coverage {
		case simulateCoverageConfirmed:
			response.CoveredTotal++
			response.ConfirmedTotal++
		case simulateCoverageHeuristic:
			// Counted as covered so CoveredTotal keeps its v1 meaning and the
			// header arithmetic stays hand-checkable, but tracked separately so
			// the verdict can state how much of it is unproven.
			response.CoveredTotal++
			response.HeuristicTotal++
		default:
			response.UncoveredTotal++
		}
		if entry.Mention {
			response.MentionTotal++
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
	response.AnalysisPartial, response.PartialReasons = simulatePartiality(response)
	// Built after the cap, so the list matches the entries actually printed. The
	// totals above already say how many were truncated away.
	response.Verify = simulateVerifications(response)
	return response
}

// simulatePartiality decides whether this verdict rests on incomplete analysis,
// and names every reason it does.
//
// It reports the reasons rather than a degree, because a degree would be a
// score and nothing here calibrates one. Each reason is a fact already printed
// elsewhere in the response, so a reader can check the flag against the body.
func simulatePartiality(response simulateResponse) (bool, []string) {
	reasons := []string{}
	scope := completenessScopeOrAll(response.CompletenessScope,
		response.Warnings, response.PartialFailures, response.Stats)
	if level := response.Stats.CompletenessLevel; level != "" && level != "ok" {
		reasons = append(reasons, fmt.Sprintf("snapshot completeness is %q, not \"ok\"", level))
	}
	if scope.LanguageFailed > 0 {
		reasons = append(reasons, fmt.Sprintf("%d file%s in the focus's own language failed to parse",
			scope.LanguageFailed, pluralSuffix(scope.LanguageFailed)))
	}
	if len(scope.InScopeWarnings) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d provider warning%s in scope for this query",
			len(scope.InScopeWarnings), pluralSuffix(len(scope.InScopeWarnings))))
	}
	if response.AffectedTruncated {
		reasons = append(reasons, fmt.Sprintf("impact listed fewer matches than it found: %d omitted, so every total here is a lower bound",
			response.AffectedOmittedTotal))
	}
	if response.HeuristicTotal > 0 {
		reasons = append(reasons, fmt.Sprintf("%d of %d covered symbol%s rest on heuristic evidence only",
			response.HeuristicTotal, response.CoveredTotal, pluralSuffix(response.CoveredTotal)))
	}
	if response.UncoveredTotal > 0 {
		reasons = append(reasons, fmt.Sprintf("%d symbol%s have no test the graph could resolve, which is not proof no test exists",
			response.UncoveredTotal, pluralSuffix(response.UncoveredTotal)))
	}
	if response.MentionTotal > 0 {
		entries := "entries are"
		if response.MentionTotal == 1 {
			entries = "entry is"
		}
		reasons = append(reasons, fmt.Sprintf("%d affected %s a lexical doc-mention impact could not resolve",
			response.MentionTotal, entries))
	}
	return len(reasons) > 0, reasons
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

	add := func(endpoint neighborEndpoint, relation string, depth int, focus bool, detail string) {
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
			Detail:      detail,
			Mention:     detail == impactMentionDetail,
		})
	}

	if impact.Focus != nil {
		add(*impact.Focus, "", 0, true, "")
	}
	for _, section := range []impactSection{impact.Callers, impact.TypeConsumers, impact.DataFlows} {
		for _, entry := range section.Entries {
			add(entry.Endpoint, entry.Relation, entry.Depth, false, entry.Detail)
		}
	}
	return out
}

// simulateAffectedSections is the exact set of impact sections this command
// treats as the blast radius. Named once so the omitted-count below cannot
// drift out of step with the walk above it.
func simulateAffectedSections(impact impactResponse) []impactSection {
	return []impactSection{impact.Callers, impact.TypeConsumers, impact.DataFlows}
}

// simulateOmittedTotal counts matches impact FOUND but did not LIST, summed over
// the sections this command reads.
//
// impactSection carries Total (every match) and Entries (the per-section cap).
// v1 iterated Entries and never read Total, so a capped section made
// AffectedTotal the cap rather than the truth while the header stated it as
// fact -- and the risk ratio was divided by that same short number. This does
// not try to reconstruct the missing symbols: they are not in the payload. It
// reports how many are missing, which is enough to know the totals are a lower
// bound.
func simulateOmittedTotal(impact impactResponse) int {
	omitted := 0
	for _, section := range simulateAffectedSections(impact) {
		if short := section.Total - len(section.Entries); short > 0 {
			omitted += short
		}
	}
	return omitted
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

	// Tier 1: a symbol in a test file has an inbound relation to this one. WHICH
	// tier that earns -- `edge` or `edge-heuristic` -- depends on the relation's
	// resolution, decided by simulateEvidenceTier. Selection is unchanged from
	// v1: if any inbound test edge exists the mirror pass is skipped, so a
	// fully resolved symbol resolves to the same test list it always did.
	for _, relation := range index.inboundByID[endpoint.ID] {
		source, ok := index.symbolsByID[relation.FromID]
		if !ok || !isConventionalTestPath(source.FilePath) || seen[source.ID] {
			continue
		}
		seen[source.ID] = true
		tests = append(tests, simulateCoveringTest{
			Endpoint: simulateEndpointOf(source),
			Relation: relation.Type,
			// The tier is decided by HOW the provider resolved this edge, not
			// by the fact that an edge exists. v1 read relation.Type alone and
			// stamped every one of these `edge`, which claimed proven execution
			// for edges drawn off a unique name -- exactly what a repository
			// using dynamic dispatch produces.
			Evidence:   simulateEvidenceTier(relation),
			Resolution: relation.Resolution,
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
		return "LOW", fmt.Sprintf("all %d affected symbol%s %s a resolved covering test",
			affectedTotal, pluralSuffix(affectedTotal), simulateHasVerb(affectedTotal))
	}
	ratio := float64(uncoveredTotal) / float64(affectedTotal)
	// "no RESOLVED covering test", not "no covering test". The graph returning
	// nothing for a symbol means either no test reaches it or the resolver could
	// not follow the path that does, and this line cannot tell them apart. The
	// thresholds below are unchanged from v1; only the claim is narrowed to what
	// the evidence supports.
	reason := fmt.Sprintf("%d of %d affected symbol%s %s no resolved covering test",
		uncoveredTotal, affectedTotal, pluralSuffix(affectedTotal), simulateHasVerb(uncoveredTotal))
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

// simulateVerification is one printed instruction that would settle one claim
// this command cannot settle itself.
//
// This is the fallback the doctrine requires: where the graph is not proof, the
// output must hand back a way to get proof. Every command below is derived
// mechanically from data already in the response -- nothing is guessed, and
// nothing runs anything.
type simulateVerification struct {
	Symbol   string `json:"symbol"`
	Location string `json:"location"`
	Why      string `json:"why"`
	Command  string `json:"command"`
}

// simulateVerifications lists, in reading order, every claim that is not
// confirmed structural evidence, with the command that would confirm it.
//
// Two shapes, because the two open questions are different:
//
//   - heuristic coverage: a test IS named, so the question is whether it really
//     exercises the symbol. Run that one test.
//   - unresolved coverage: no test was resolved, so the question is what the
//     graph actually holds for this symbol. `neighbors --direction in` prints
//     every inbound edge WITH its resolution, including the weak ones this
//     command declined to count.
func simulateVerifications(response simulateResponse) []simulateVerification {
	out := []simulateVerification{}
	for _, entry := range response.Affected {
		switch entry.Coverage {
		case simulateCoverageConfirmed:
			continue
		case simulateCoverageHeuristic:
			test := entry.Tests[0]
			why := fmt.Sprintf("%s evidence", test.Evidence)
			if test.Resolution != "" {
				why = fmt.Sprintf("%s evidence, resolution=%s", test.Evidence, test.Resolution)
			}
			out = append(out, simulateVerification{
				Symbol:   entry.Endpoint.Name,
				Location: simulateLocation(entry.Endpoint),
				Why:      why,
				Command:  simulateTestCommand(test.Endpoint),
			})
		default:
			why := "no test the graph could resolve"
			if entry.Mention {
				why = "impact could not resolve this entry: " + entry.Detail
			}
			out = append(out, simulateVerification{
				Symbol:   entry.Endpoint.Name,
				Location: simulateLocation(entry.Endpoint),
				Why:      why,
				Command:  simulateInboundCommand(entry.Endpoint),
			})
		}
	}
	return out
}

// simulateTestCommand names the single test to run to settle a heuristic claim.
//
// The Go form is emitted only for a Go test file, where the package path is the
// directory and `-run` takes the function name -- both mechanical. For any other
// language the runner is NOT guessed: inventing `pytest` or `rspec` invocations
// for a project whose layout we have not seen would be exactly the unearned
// confidence this change exists to remove, so those fall back to the graph
// command, which is always correct.
func simulateTestCommand(test neighborEndpoint) string {
	if test.Language == "Go" && test.Name != "" && strings.HasPrefix(test.Name, "Test") {
		module := simulateModuleOf(test.FilePath)
		if module == "" {
			module = "."
		}
		return fmt.Sprintf("go test ./%s/ -run '^%s$' -count=1", strings.Trim(module, "/"), test.Name)
	}
	if test.FilePath == "" {
		return "open the cited test and confirm it exercises this symbol"
	}
	return fmt.Sprintf("open %s and confirm it exercises this symbol", simulateLocation(test))
}

// simulateInboundCommand prints every inbound edge the graph holds for a symbol,
// with the resolution of each -- the question behind an unresolved verdict.
func simulateInboundCommand(endpoint neighborEndpoint) string {
	name := endpoint.Name
	if name == "" {
		return "entire graph neighbors --repo . --relation CALLS --direction in"
	}
	return fmt.Sprintf("entire graph neighbors --repo . --symbol %s --relation CALLS --direction in", name)
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

// simulateHasVerb agrees the verb with its count. A report whose entire claim
// is care cannot open with "1 affected symbols have".
func simulateHasVerb(count int) string {
	if count == 1 {
		return "has"
	}
	return "have"
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
	// The same banner impact prints, from the same writer and the same scope.
	// v1 carried these diagnostics in the payload and rendered none of them, so
	// a snapshot the provider called "degraded" still produced a bare RISK line.
	// writeScopedCompletenessBlock is silent when there is nothing to say, which
	// is what keeps a clean repository's output unchanged.
	writeScopedCompletenessBlock(writer,
		completenessScopeOrAll(response.CompletenessScope, response.Warnings,
			response.PartialFailures, response.Stats),
		response.Warnings, response.PartialFailures, response.Stats)

	// The omitted count is stated ON the header it invalidates. A reader who
	// sees only "AFFECTED 9" must not have to look elsewhere to learn that 9 is
	// a floor rather than the count.
	truncation := ""
	if response.AffectedTruncated {
		truncation = fmt.Sprintf("  [at least %d more not listed by impact's per-section cap; every total below is a lower bound]",
			response.AffectedOmittedTotal)
	}
	fmt.Fprintf(writer, "\nAFFECTED   %d symbol%s across %d module%s%s\n",
		response.AffectedTotal, pluralSuffix(response.AffectedTotal),
		len(response.Modules), pluralSuffix(len(response.Modules)), truncation)
	for _, module := range response.Modules {
		fmt.Fprintf(writer, "  %s\n", module)
	}

	var uncovered []simulateAffected
	covered := 0
	// "a resolved covering test", and then the split. One number saying
	// "6 of 12 covered" invites the reading that six are safe; the second line
	// says how many of those six are actually proven.
	fmt.Fprintf(writer, "\nTESTS      %d of %d affected symbol%s %s a resolved covering test\n",
		response.CoveredTotal, response.AffectedTotal,
		pluralSuffix(response.AffectedTotal), simulateHasVerb(response.CoveredTotal))
	// Only worth a line when there is a split to report. On a fully resolved
	// answer this is silent, which is what keeps a clean repository's output
	// from growing noise it cannot act on.
	if response.CoveredTotal > 0 {
		fmt.Fprintf(writer, "           %d confirmed by a resolved edge, %d on heuristic evidence only\n",
			response.ConfirmedTotal, response.HeuristicTotal)
	}
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
		// The resolution is printed beside the tier so the tier is auditable
		// rather than asserted: `edge [exact]` and `edge-heuristic [name_only]`
		// let a reader see WHY the label is what it is.
		resolution := ""
		if test.Resolution != "" {
			resolution = "[" + test.Resolution + "]"
		}
		fmt.Fprintf(writer, "  %-40s %-15s %-12s %s -> %s%s\n",
			simulateCell(simulateLocation(test.Endpoint), 40), test.Evidence, resolution,
			test.Endpoint.Name, entry.Endpoint.Name, extra)
	}
	if covered == 0 {
		fmt.Fprintln(writer, "  (none)")
	}

	// BREAKING names the affected symbols outside the focus's own module. A
	// developer reading their own diff sees the same-module callers for free;
	// these are the ones they will not notice. Cross-module AND uncovered is
	// the worst case, so it is marked inline rather than left to be inferred.
	fmt.Fprintf(writer, "\nBREAKING   %d affected %s outside %s (%d with no resolved test)\n",
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
			marker = "  ** no resolved test **"
		}
		fmt.Fprintf(writer, "  %-30s %s%s\n", entry.Endpoint.Name, simulateLocation(entry.Endpoint), marker)
	}
	if !printedBreaking {
		fmt.Fprintln(writer, "  (none - the blast radius stays inside one module)")
	}

	// UNCOVERED is the finding. It is printed last so it is what remains on
	// screen, and it names a location per line because the only useful next
	// action is to open one and write a test.
	// The claim is "no test the graph could RESOLVE". The strong reading -- no
	// test exists -- is not available from static analysis of a repository using
	// dynamic dispatch, reflection or generated code, and stating it would be
	// the overclaim this whole section exists to avoid. Each of these symbols
	// gets a command in VERIFY below.
	fmt.Fprintf(writer, "\nUNCOVERED  %d affected symbol%s %s no test the graph could resolve\n",
		response.UncoveredTotal, pluralSuffix(response.UncoveredTotal),
		simulateHasVerb(response.UncoveredTotal))
	// The caveat belongs to a claim. With nothing in this section there is no
	// claim to qualify, and printing it anyway would train readers to skip it.
	if response.UncoveredTotal > 0 {
		fmt.Fprintln(writer, "           (that is an absence of resolved evidence, not proof no test exists)")
	}
	for _, entry := range uncovered {
		marker := ""
		if entry.Focus {
			marker = "  <- the symbol you are changing"
		}
		if entry.Mention {
			marker += "  [" + entry.Detail + "]"
		}
		fmt.Fprintf(writer, "  %-30s %s%s\n", entry.Endpoint.Name, simulateLocation(entry.Endpoint), marker)
	}
	if len(uncovered) == 0 {
		fmt.Fprintln(writer, "  (none)")
	}

	// VERIFY is the fallback path. Everything above that is not confirmed
	// structural evidence appears here with the command that settles it, so a
	// reader is never left holding an unproven claim and no way to check it.
	if len(response.Verify) > 0 {
		claims := "claims are"
		if len(response.Verify) == 1 {
			claims = "claim is"
		}
		fmt.Fprintf(writer, "\nVERIFY     %d %s not confirmed structural evidence\n",
			len(response.Verify), claims)
		for _, item := range response.Verify {
			fmt.Fprintf(writer, "  %-30s %-40s %s\n",
				simulateCell(item.Symbol, 30), simulateCell(item.Location, 40), item.Why)
			fmt.Fprintf(writer, "      $ %s\n", item.Command)
		}
	}

	fmt.Fprintf(writer, "\nRISK  %s - %s\n", response.Risk, response.RiskReason)
	// The bound, stated as arithmetic a reader can redo: if none of the
	// heuristic edges hold, this many are unguarded. No score, no percentage --
	// just the two numbers already printed above, added.
	if response.HeuristicTotal > 0 {
		verb := "rest"
		if response.HeuristicTotal == 1 {
			verb = "rests"
		}
		fmt.Fprintf(writer, "      %d covered symbol%s %s on heuristic evidence; if none hold, %d of %d are unguarded.\n",
			response.HeuristicTotal, pluralSuffix(response.HeuristicTotal), verb,
			response.UncoveredTotal+response.HeuristicTotal, response.AffectedTotal)
	}

	// The partiality block is last and unmissable. It states, in the reader's
	// own terms, every input to the verdict above that was incomplete.
	if response.AnalysisPartial {
		fmt.Fprintln(writer, "\nANALYSIS MAY BE PARTIAL - this verdict rests on incomplete evidence:")
		for _, reason := range response.PartialReasons {
			fmt.Fprintf(writer, "  - %s\n", reason)
		}
	}
	if response.TestsHeuristic {
		fmt.Fprintln(writer, "Note: TESTS is a heuristic relation in this provider, and static analysis")
		fmt.Fprintln(writer, "cannot resolve dynamic dispatch, reflection or generated code.")
		if len(response.Verify) > 0 {
			fmt.Fprintln(writer, "Open the cited lines, or run the VERIFY commands, before gating a merge.")
		} else {
			fmt.Fprintln(writer, "Every claim above rests on a resolved edge; open the cited lines to confirm.")
		}
	}
}

// simulateCell keeps a column aligned when its value overflows, WITHOUT ever
// hiding a line number: truncation happens on the left, so the `file:line` tail
// of a long path survives. A report whose whole claim is that every finding is
// checkable cannot afford to elide the part a reader needs to open the file.
func simulateCell(value string, width int) string {
	if width < 4 || len(value) <= width {
		return value
	}
	return "..." + value[len(value)-(width-3):]
}
