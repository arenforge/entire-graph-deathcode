# graph simulate — coverage-aware change simulation you are allowed to distrust

## One-sentence summary

`entire graph simulate` takes the blast radius `impact` already computes and
answers the question it leaves open — *of everything my change can reach, which
parts does a test actually guard?* — labelling every claim with the strength of
the evidence behind it, and handing back a command for every claim it cannot
prove.

## Problem, intended user and why it matters

The user is a developer about to change a function, or a reviewer reading a
change an agent wrote. Two questions decide whether it is safe to merge: *what
else depends on this*, and *would anything catch me if I got it wrong?*

This repository already answered the first. `entire graph impact` reports
callers, callees, type consumers and data flows for any symbol. Nothing
answered the second, and the pieces were sitting right next to each other:

- `internal/sem/search_covertest.go` implements a tiered covering-test resolver
  — but it is wired only into `search`, returns exactly **one** test, and only
  for the **top hit**.
- `internal/cli/impact.go` has **zero** mentions of `TESTS`. The blast-radius
  command has no notion of coverage at all.

So the tool could say "here is one test for one symbol" and "here is everything
that depends on one symbol", and nothing composed them. A blast radius of
twelve symbols arrived with no indication of which six were unguarded. That
composition is this command.

## Selected Entire track and why Entire is essential

**Track 2 — Build with Graph Intelligence.**

The product cannot exist without the graph. Mapping a symbol to its transitive
callers, then those callers to the test functions that reach them, is a
call-graph query, not a text query. Entire Graph supplies it locally, with no
model calls, and returns `file:line` for every node — which is what makes the
verdict auditable rather than advisory.

The graph is also what chose the feature. We did not guess at a gap; we found
it with `entire graph search` and confirmed it by opening the cited lines.

## Architecture and main workflow

```
--symbol NAME | <file>:<line>
      │
      ▼  shared focus resolver (ambiguity answered exactly as `impact` answers it)
parseImpactFlags                                internal/cli/simulate.go:348
      │
      ▼  ONE snapshot load, no network
sem.LoadOrBuildProviderSnapshot                 internal/cli/simulate.go:305
      │
      ├─▶ blast radius, REUSED not recomputed
      │   buildImpactResponseFromReader         internal/cli/impact.go:330
      │
      ├─▶ coverage index, built once per run (not once per symbol)
      │   newSimulateCoverageIndex              internal/cli/simulate.go:584
      │
      ▼  correlate, tier by resolution strength, classify
buildSimulateResponse                           internal/cli/simulate.go:360
      │
      ▼
AFFECTED / TESTS / BREAKING / UNCOVERED / VERIFY / RISK  (text | json)
writeSimulateText                               internal/cli/simulate.go:939
```

| File | What |
| --- | --- |
| `internal/cli/simulate.go` | The whole command |
| `internal/cli/simulate_test.go` | 17 tests, including the dynamic-dispatch fixture |
| `internal/cli/root.go:104` | `case "simulate":` |
| `internal/cli/help.go:207` | Help entry |
| `internal/cli/completeness.go` | Reused for partial-analysis reporting, not modified |
| `docs/buildathon/evidence/` | Captured runs |

Two reuse decisions worth stating, because they are load-bearing:

**The blast radius is `impact`'s, unchanged.** If `simulate` recomputed it, the
two commands could disagree about what "affected" means and a developer who ran
both would have no way to know which to believe.

**Partial-analysis reporting is `completeness.go`'s, unchanged.** `impact`
builds a query-scoped completeness view and renders it. Rather than write a
second one, `simulate` now carries the same scope and calls the same writer.

## Entire Graph findings and verification

Captured runs live in [`docs/buildathon/evidence/`](docs/buildathon/evidence/).

| # | Requirement | Artifact |
| --- | --- | --- |
| 1 | Graph search / definition lookup | `01-graph-search.txt` |
| 2 | Impact analysis before a high-risk change | `02-impact-pre-change.txt` |
| 3 | The command running on real code | `03-simulate-run.txt` |
| 4 | **Impact analysis before the curveball edit** | `04-before-curveball.txt` |
| 5 | The incomplete-analysis fixture, with ground truth | `06-curveball-fixture.txt` |
| 6 | Final semantic diff of the submitted implementation | `05-final-semantic-diff.txt` |

Every finding below was checked against source before we relied on it.

**Finding 1 — the covering-test resolver exists but is not composed.**
`internal/sem/search_covertest.go` ranks coverage evidence by tier
(`edge` > `mirror`), and `search` uses it to name one test for the top hit.
Verified by reading the file and by grepping `internal/cli/impact.go` for
`TESTS`: zero matches.

**Finding 2 — `TESTS` is a heuristic relation, and thin.** The provider lists
`TESTS` in `HeuristicRelationTypes`. This repository contains **9** `TESTS`
edges against **19,465** `CALLS`. So almost all coverage evidence is "a call
originating in a test file", not an explicit `TESTS` edge. We print the
relation type on every line rather than let a reader assume otherwise.

**Finding 3 — and the one that drove the curveball response — not one of those
9 `TESTS` edges is exactly resolved.** Eight are `name_only`, one is `package`.
Measured with `./entire-graph edges`, recorded in
`04-before-curveball.txt` §D2.

**Finding 4 — `simulate` was dropping the partial-analysis signal `impact`
prints.** On the same snapshot, back to back, `impact` printed
`Completeness: degraded for Go (...)` on all 8 runs and `simulate` printed it
**0** times. The diagnostics were plumbed into the response and never read.
Recorded, with the exact recompute commands, in `04-before-curveball.txt` §D1.

## Noon Curveball: what changed and how we adapted

**The constraint.** *The graph is evidence, not an oracle.* Our tool had hit
repositories using dynamic dispatch, reflection or generated code, which static
analysis cannot fully resolve. It had to stop presenting incomplete
relationships as certain, identify when analysis may be partial, offer a
verification path, keep working for fully resolved code, ship a fixture
representing incomplete analysis, and let a reader tell three classes of
evidence apart.

**The assumption it invalidated.** `simulate` treated the graph as a **closed
world**: an absent relation was read as a negative fact about the code, and any
present relation as proof of execution. Both halves lived in one function —
`len(entry.Tests) == 0` became the affirmative claim "has no covering test" and
drove the `RISK` verdict, while any inbound edge from a test file was stamped
tier `edge`, documented as *"the graph proved a test actually runs this code"*.

Dynamic dispatch produces exactly the two states that assumption has no room
for: a real test whose path the resolver cannot follow, and an edge drawn only
because one name happened to match.

**We ran the graph on our own code before editing any of it.** `impact` and
`simulate` on all eight of our graph-evidence consumers, saved to
`04-before-curveball.txt`. That is what located the four defects above and
bounded the change to the sites it names.

**What changed.**

1. **Evidence is tiered by *resolution*, not by relation type.** A test edge is
   `edge` only when the provider's own `Resolution` says the call was traced
   (`exact`, `import_resolved`, `package`); a name, pattern or inferred-type
   match is `edge-heuristic`. The partition is read off the provider's `Reason`
   strings and documented at `simulateConfirmedResolution`. An unknown
   resolution degrades toward verification, never toward proof.
2. **Three coverage states**, in text and JSON: `confirmed` / `heuristic` /
   `unresolved`. `unresolved` is never called "no test exists".
3. **The partial-analysis banner is rendered**, via the existing
   `writeScopedCompletenessBlock`, plus an `ANALYSIS MAY BE PARTIAL` block that
   names every reason and an `analysis_partial` boolean for gates.
4. **Truncation is surfaced.** `impact` reports `Total` and a capped `Entries`;
   we now report how many matches were omitted and say the totals are a lower
   bound, on the header they qualify.
5. **A `VERIFY` section**: every claim that is not confirmed structural
   evidence, with the command that settles it — the one test to run for a
   heuristic claim, `neighbors --direction in` for an unresolved one. The Go
   test invocation is emitted only for Go test files; for other languages the
   runner is not guessed.
6. **`impact`'s own doc-mention label is carried through** instead of dropped.

**What did not change**, and is pinned by test: for a symbol whose test edge is
resolved, the tier is still `edge`, the verdict is `confirmed`, the
covered/uncovered classification is identical, and the `RISK` thresholds are
untouched. In this repository that is **1292 of 1405** covered symbols. The
defect affected **113**.

**The fixture.** A repository where `Gateway` has two implementations, so no
unique implementing method exists and the interface call cannot be resolved.
`TestCheckoutWithStripe` genuinely executes `chargeViaStripe` — Go's own
coverage tool reports it **100% covered** — and the graph cannot see it. The
old output said "no covering test", which was simply false. The new output says
no test could be *resolved*, flags the analysis as partial, and prints the
command to check. Both states are asserted in
`TestSimulateOnUnresolvableDynamicDispatchFixture`.

## Checkpoint links and what each checkpoint proves

| Milestone | Commit | What it proves |
| --- | --- | --- |
| Initial understanding and intended architecture | `651d2a0` | The gap was found with the graph and confirmed against source; the rejected alternatives are recorded with reasons |
| Cross-module blast radius | `b9dbba6` | Last stable state before the Curveball, with open risks listed |
| Found by running it | `dca1066` | `"tests": []` vs `null` are different claims; caught by a teammate's real run |
| Plain-language handover | `df3bf1b` | Three people can present work they did not personally write |
| Response to the Curveball | this commit | Graph analysis captured *before* the edit; the closed-world assumption removed; fixture with measured ground truth |

## Verification

```
go build ./...                             clean
gofmt -l ./cmd ./internal                  clean
go vet ./internal/cli/                     clean
go test -timeout 40m ./...                 ok, all 8 packages, exit 0
```

The full suite passing is new. It had never been run on this branch, and doing
so surfaced a real pre-existing bug: `simulate` was in `root.go`'s dispatch
switch and in `help.go`'s registry but missing from the `dispatchCommands`
mirror list in `help_test.go`, so `TestRegistryMatchesDispatch` had been failing
since the command was first added. Fixed here, one line.

## Setup, run and test instructions

```sh
git clone entire://aws-ap-south-1.entire.io/gh/arenforge/entire-graph-deathcode
cd entire-graph-deathcode
go build -o entire-graph ./cmd/entire-graph

# the new command. --head and --cache-dir are both required for the index
# cache to hit: 22.0s on the first run, 0.637s on every run after.
# Without --head the working tree is read, which cannot be cached.
./entire-graph simulate --repo . --symbol buildImpactResponseFromReader \
    --head --cache-dir .cache/graph

# machine output, including analysis_partial and the verify list
./entire-graph simulate --repo . --symbol FormatAmount --format json | jq .

# tests (the env vars keep fixtures from inheriting global git config)
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
  go test ./internal/cli/ -run 'Simulate|CoveringTests|CollectSimulate' -count=1
```

Toolchain: Go 1.26, Git >= 2.36, Entire CLI >= 0.10.0, Entire Graph v0.4.0.

## Databricks use, data sources and limitations

Not applicable — this team did not opt in to the Databricks award category.

## Known limitations and next steps

State these before a judge finds them.

1. **"Confirmed" means a test *reaches* the symbol, not that it *asserts the
   behaviour you are changing*.** This is the honest ceiling of the whole
   approach. The tier says the call was traced; it says nothing about the
   assertion.
2. **`TESTS` is a heuristic relation type**, and this repository has 9 of them,
   all resolved `name_only` or `package`. The command prints this itself.
3. **The confirmed/heuristic partition is a judgement, even though it is read
   off the provider.** `package` counts as confirmed because its reason string
   is *"direct call expression resolved to same-package symbol"*;
   `type_inferred` does not, because the receiver type was inferred. Both calls
   are defensible and both are pinned by
   `TestSimulateConfirmedResolutionPartition`, which is where the argument
   should be had if someone disagrees.
4. **Unresolved does not distinguish "no test" from "test we cannot see".** We
   report the ambiguity honestly but we cannot resolve it; that is what `VERIFY`
   is for. A future version could run the cited tests under coverage and close
   the loop.
5. **Verified on this repository, plus a Go fixture.** Stem matching is
   unit-tested against TS/Python/Ruby naming conventions, and the resolution
   partition is language-independent, but neither has been exercised on a real
   project in those languages.
6. **The Go `-run` verification command is emitted only for Go.** For other
   languages we print "open this file" rather than guess at a test runner.
7. **Cold start is ~22 seconds**, and the cache only hits with BOTH `--head`
   and `--cache-dir`. Without `--head` the working tree is read, which cannot be
   cached, so warming has no effect and every run pays full cost. Measured on
   this repository: 22.0s cold, 0.637s warm.
8. **Symbols at the repository root report module `""`**, so a root-level
   fixture prints "across 0 modules". Cosmetic, pre-existing, and untouched
   here because changing the module grouping would change `BREAKING`.
9. **`--limit` truncates the listing after the totals are computed**, so counts
   stay true while the display shrinks; uncovered entries survive truncation
   first.
