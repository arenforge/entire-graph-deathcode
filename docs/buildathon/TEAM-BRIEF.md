# Team brief — read this first

Everything the team needs to understand what we built, why, and what is left.
Written in plain language on purpose. No prior context needed.

---

## 1. Where the project is

**Use this folder:**

```
/Users/arhan/1. New Study Material/Scaler Buildathon/deathcode
```

**Do NOT use** `Scaler Buildathon/entire-graph-deathcode` (an old plain clone,
abandoned) or the outer `Scaler Buildathon` folder (an empty repo created by
accident).

Why it matters: the Buildathon rules say all implementation must happen in the
clone created through the Entire mirror workflow. Only `deathcode` is that
clone. Its `origin` is `entire://aws-ap-south-1.entire.io/...`, not a
`github.com` URL. That is how you can tell you are in the right place.

Branch: `buildathon/main`

### Get set up (each person, once)

```sh
cd "/Users/arhan/1. New Study Material/Scaler Buildathon/deathcode"
git pull
go build -o entire-graph ./cmd/entire-graph

# Run it ONCE to build the index (~22s), then every later run is ~0.6s.
# --head and --cache-dir are BOTH required for the cache to hit.
./entire-graph simulate --repo . --symbol buildImpactResponseFromReader \
    --head --cache-dir .cache/graph
```

**Do not drop `--head`.** Without it the tool reads your working tree, which
cannot be cached, so every single run pays the full 22 seconds. With it, the
second run onwards is 0.6s. Measured: 22.0s cold, 0.637s warm.

If you see a report with the words AFFECTED, TESTS, BREAKING, UNCOVERED and
RISK, you are ready.

---

## 2. What this repository already was, before we touched it

It is **not** a blank starter app. It is a real, mature tool: the **Entire
Graph** plugin, written in Go, around 20 programming languages supported. It
reads a codebase locally using tree-sitter and builds a map of it — which
function calls which, which types are used where, which file defines what.

Two things about it shape everything we did:

1. **It makes no AI/model calls and no network requests.** All the analysis is
   deterministic parsing. This is a selling point the project states loudly.
2. **It is held to a high standard.** Roughly 60% of the code in `internal/cli`
   is tests. So anything we add needs implementation **plus** tests **plus**
   docs, or it looks out of place.

### The commands it already had

- `search` — find code by asking a question in plain language
- `def` — show one thing's definition
- `neighbors` — what is directly connected to this symbol
- **`impact`** — "if I change this function, what else is affected?"
- `diff` / `commit` — what changed between two versions
- `snapshot` / `symbols` / `edges` — dump the raw graph

---

## 3. The gap we found (this is our whole project)

We searched the code before deciding anything. Here is what we found.

**Finding A.** The file `internal/sem/search_covertest.go` already contains
smart logic that answers *"which test covers this piece of code?"* It ranks its
answer by how strong the evidence is:

| Tier | What it means | Strength |
| --- | --- | --- |
| `edge` | The code graph proved a test actually calls/runs this code | Strongest |
| `mirror` | A test sits in the matching test file (`foo_test.go` next to `foo.go`) **and** mentions this code by name | Weaker |

**Finding B.** That logic is only wired into the `search` command, it returns
**exactly one** test, and only for the **top search result**.

**Finding C.** We grepped `internal/cli/impact.go` — the blast-radius command —
for any mention of tests. **Zero matches.** It has no idea about test coverage
at all.

### So here is the hole

The tool could tell you:

- "here is one test for one piece of code" (in `search`), and
- "here is everything that depends on one function" (in `impact`)

...but **nothing joined those two together.** Nobody could answer the question
a developer actually stops on:

> *"Of everything my change can break, which parts does a test actually guard —
> and which parts are naked?"*

That question is our product.

---

## 4. What we built: `entire graph simulate`

### Run it

```sh
./entire-graph simulate --repo . --symbol NAME_OF_A_FUNCTION
```

Add `--format json` for machine output.

### What it prints, explained line by line

```
Change simulation: buildImpactResponseFromReader  internal/cli/impact.go:330
Index: cache-hit
```
The function you asked about, and where it lives. `cache-hit` means it used a
saved index (fast). `cache-miss` means it had to re-read the repo (~21 seconds).

```
AFFECTED   12 symbols across 2 module(s)
  internal/cli
  internal/sem
```
Twelve things in the codebase are touched if you change this function, spread
across two folders.

```
TESTS      6 of 12 affected symbols have a resolved covering test
           5 confirmed by a resolved edge, 1 on heuristic evidence only
  internal/cli/linereader_ondisk_test.go:23  edge  [package]  buildImpactResponseOnDisk -> buildImpactResponseFromReader
```
Six of the twelve have a test the graph could resolve. Each line reads:
**where the test lives** → **evidence tier** → **how the graph resolved it** →
**test name** → **what it covers**.

The second line is the important one. `edge` means the provider traced the
call. `edge-heuristic` means the edge exists but was drawn by matching a
**name**, not by following a call — which is what dynamic dispatch produces.
Same for `mirror`. So "6 covered" splits into "5 proven, 1 believed".

```
BREAKING   1 affected symbol is outside internal/cli (0 uncovered)
  ProviderSnapshot               internal/sem/provider.go:417
```
This affected code lives in a **different folder** from the function you are
changing. These are the ones you would never notice while reading your own
diff — someone else's code depends on you.

```
UNCOVERED  6 affected symbols have no test the graph could resolve
           (that is an absence of resolved evidence, not proof no test exists)
  runImpact                      internal/cli/impact.go:124
  ...
```
**This is the product.** Six affected things have no test the graph can find.
Change your function, break these, and probably nothing catches you.

Read the wording carefully, because a judge will. We do **not** say "no test
exists". If a test reaches this code through an interface, reflection or
generated code, static analysis cannot see it and we would be lying. We say
what we actually know: nothing was **resolved**.

```
VERIFY     7 claims are not confirmed structural evidence
  runImpact       internal/cli/impact.go:124   no test the graph could resolve
      $ entire graph neighbors --repo . --symbol runImpact --relation CALLS --direction in
```
Every claim we cannot prove ships with the command that settles it. For a
heuristic claim it is the one test to run; for an unresolved one it is the query
that shows every inbound edge the graph actually holds, weak ones included.

```
RISK  HIGH - 6 of 12 affected symbols have no resolved covering test (>= 5 uncovered)
      1 covered symbol rests on heuristic evidence; if none hold, 7 of 12 are unguarded.
```
A four-level verdict: NONE / LOW / MEDIUM / HIGH. **Not a made-up score** — you
can recount it from the two numbers printed above it. The second line is the
same arithmetic run against the weak evidence: 6 + 1 = 7.

```
ANALYSIS MAY BE PARTIAL - this verdict rests on incomplete evidence:
  - snapshot completeness is "degraded", not "ok"
  - 1 provider warning in scope for this query
  - 1 of 6 covered symbols rest on heuristic evidence only
```
Fires whenever any input to the verdict was incomplete, and **names every
reason** so you can check the flag against the body of the report. In JSON it
is `analysis_partial` plus `partial_reasons`, so a merge gate can read it
without parsing text.

---

## 5. Design decisions, and why (judges will ask)

### Why callees are excluded
If function A calls function B, and you change A, **B does not break.** So we
count only things that depend on you: callers, type consumers, and data flows.
Not callees.

### Why we ignore "co-change files" and "siblings"
The `impact` command reports files that historically changed together. That is a
*correlation*, not a dependency. If we reported "no test covers this" about
something your change cannot actually break, that is a false alarm — and one
false alarm makes a reviewer stop trusting the whole report.

### Why the function you are changing is in the list
It is the thing being edited. Whether a test guards *it* is the first thing you
want to know. Leaving it out would report coverage on everything except the
subject.

### Why we reuse `impact`'s blast radius instead of computing our own
If we recomputed it, `impact` and `simulate` could disagree about what
"affected" means, and a developer who ran both would have no way to know which
to believe. So we call the exact same function `impact` calls.

### Why the mirror tier requires the test to name the code
Otherwise every test in `foo_test.go` would get credit for covering every
function in `foo.go`. That is exactly the kind of fake coverage claim this tool
exists to *expose*. So filename adjacency alone is never enough.

### Things we deliberately did NOT build

| Rejected | Why |
| --- | --- |
| A score like "Risk 8.9/10" or "Confidence 94%" | We have no calibrated basis for a decimal. This repo's own README spends 40 lines retracting over-precise benchmark numbers — inventing one here would be indefensible. |
| An LLM in the decision path | This tool "makes no network requests, model calls, or API-key lookups". Adding AI would contradict the product we are extending. All our logic is deterministic. |
| A deployment checklist | Nothing in a call graph knows your deploy process. It would be invented prose. |
| Suggested reviewers | Comes from `git blame`, not the graph. `impact` already reports co-change files, which is most of it. Listed as a next step instead. |
| Leading with blast radius / affected modules | `impact` already ships those. Re-printing them as our headline would be a reskin, not a new capability. |

---

## 6. Files we changed

| File | What |
| --- | --- |
| `internal/cli/simulate.go` | **New.** The whole command, heavily commented |
| `internal/cli/simulate_test.go` | **New.** 17 tests, incl. the dynamic-dispatch fixture |
| `internal/cli/completeness.go` | **Reused unchanged** for partial-analysis reporting |
| `internal/cli/root.go` | One line: `case "simulate":` added to the command switch |
| `internal/cli/help.go` | The help-text entry so `entire graph help` lists it |
| `BUILDATHON.md` | Submission doc (still has TODOs) |
| `docs/buildathon/evidence/` | Captured command outputs, required by the rules |

### The tests, and what each protects

| Test | Protects |
| --- | --- |
| `TestSimulateRiskVerdicts` | The HIGH/MEDIUM/LOW boundaries cannot silently shift |
| `TestSimulateFileStemPairsSourceWithTest` | `foo.go`↔`foo_test.go`, `login.ts`↔`login.test.ts`, `session.py`↔`test_session.py`, `parser.rb`↔`parser_spec.rb` all pair up |
| `TestCapSimulateAffectedKeepsUncoveredFirst` | Truncating the list never hides the uncovered findings |
| `TestCollectSimulateAffectedDropsTestsAndExternals` | A test file is never reported as "uncovered production code" |
| `TestCollectSimulateAffectedMarksCrossModule` | The focus is never flagged as breaking; same-folder callers are not miscounted |
| `TestSimulateUncoveredSerializesEmptyTestArray` | JSON says `"tests": []`, never `null` |

Added by the curveball response:

| Test | Protects |
| --- | --- |
| `TestCoveringTestsWithholdsEdgeTierFromNameOnlyRelations` | A name-matched edge can never be labelled as proven execution |
| `TestCoveringTestsWithholdsEdgeTierFromHeuristicRelationTypes` | A `TESTS` edge cannot reach the confirmed tier — the relation type is itself heuristic |
| `TestCoveringTestsKeepsEdgeTierForResolvedRelations` | **Requirement 4**: resolved code still reads exactly as before |
| `TestSimulateConfirmedResolutionPartition` | The confirmed/heuristic split, incl. that an *unknown* resolution degrades toward verification |
| `TestSimulateCoverageVerdictThreeStates` | The three states stay distinguishable |
| `TestSimulateOmittedTotalCountsWhatImpactDidNotList` | A truncated blast radius cannot masquerade as complete |
| `TestCollectSimulateAffectedCarriesImpactMentionLabel` | `impact`'s doc-mention label is not dropped |
| `TestSimulateRiskThresholdsDidNotMove` | The risk boundaries did not shift when the wording changed |
| `TestSimulateOnUnresolvableDynamicDispatchFixture` | **The fixture.** A repo where the graph is genuinely blind, plus the resolved case in the same run |

Run them:
```sh
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
  go test ./internal/cli/ -run 'Simulate|CoveringTests|CollectSimulate' -count=1
```

---

## 7. Known limitations — say these before a judge finds them

1. **"Confirmed" means a test *reaches* the code, not that it *checks the
   behaviour* you are changing.** This is the big one. Do not let the demo imply
   the stronger claim.
2. **The graph has only 9 explicit `TESTS` edges** in this repo, against 19,465
   `CALLS` edges — and **not one of the 9 is exactly resolved** (8 `name_only`,
   1 `package`). That is why `edge` is now earned by resolution rather than by
   relation type. Say this before someone greps for it.
3. **`TESTS` is a heuristic relation type** in this provider. The tool prints
   this warning itself, on every run.
4. **"Unresolved" does not tell you whether a test exists.** It tells you the
   graph could not find one. We cannot close that gap — that is what `VERIFY`
   is for.
5. **The confirmed/heuristic split is a judgement**, even though every value in
   it is read off the provider's own reason strings. `package` counts as
   confirmed ("direct call expression resolved to same-package symbol");
   `type_inferred` does not (the receiver type was inferred). Pinned by
   `TestSimulateConfirmedResolutionPartition` — that is where to argue about it.
6. **Verified on this repository plus a Go fixture.** Stem matching is unit-
   tested against TS/Python/Ruby naming and the resolution partition is
   language-independent, but neither has run on a real project in those
   languages.
7. **The `go test -run` verification command is emitted for Go only.** For other
   languages we print "open this file" rather than guess a test runner.
8. **Cold start is ~21 seconds.** Warm the cache before demoing.
9. **Root-level files report module `""`**, so a fixture at the repo root prints
   "across 0 modules". Cosmetic and pre-existing; untouched because changing the
   module grouping would change `BREAKING`.

---

## 8. What is left to do

| Who | Task |
| --- | --- |
| P2 | JSON contract test (assert `format_version` is 2, plus `affected_total`, `uncovered_total`, `tests_heuristic`, `analysis_partial`, `partial_reasons`, `verify`) |
| P3 | Implementation order: topological sort of the affected subgraph so it says what to change first |
| P4 | `BUILDATHON.md` is now written — read it and check every claim in it against the code |
| Lead | Final semantic diff evidence at ~2:30, submit by 2:50 |

**Done since this brief was first written:**

- The noon curveball response (see §9).
- `BUILDATHON.md` rewritten. It previously described a command called `review`
  in `internal/cli/review.go` — neither exists. The submission doc was
  describing a product we did not build.
- **The full test suite now passes on this branch**, which clears the biggest
  open risk in the earlier version of this brief. It also caught a real
  pre-existing bug: `simulate` was registered in `root.go`'s dispatch switch and
  in `help.go`'s registry, but never added to the `dispatchCommands` mirror list
  in `help_test.go`, so `TestRegistryMatchesDispatch` had been failing since the
  command was added. One line, fixed here.

```sh
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null go test -timeout 40m ./...
# ok  cmd/entire-graph · cmd/graph-bench · internal/bench · internal/cli
# ok  internal/filedigest · internal/gitutil · internal/sem · internal/termsafe
```

**Full suite** (takes a while, start it and keep working):
```sh
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null go test -timeout 30m ./...
```

**Final evidence** (required by the rules):
```sh
./entire-graph diff --base 3a2a715 --head HEAD > docs/buildathon/evidence/05-final-semantic-diff.txt
```

---

## 9. The noon curveball — DONE. Here is what happened.

**The constraint:** *the graph is evidence, not an oracle.* Our tool has to
cope with repositories using dynamic dispatch, reflection or generated code,
which static analysis cannot fully resolve.

**The process, which is graded as much as the code:**

1. The previous session was sealed before the card was read, so a fresh agent
   session rebuilt its understanding from `entire checkpoint list` /
   `entire checkpoint explain` and our commit messages. It worked — our commit
   bodies are dense on purpose.
2. **Graph analysis ran BEFORE any edit**, on our own code paths that consume
   graph evidence, saved to
   [`evidence/04-before-curveball.txt`](evidence/04-before-curveball.txt) (689
   lines). Judges score graph use that happens after implementation in the
   partial band, so this ordering mattered.
3. Only then did we change code.

**The bug it found.** We were treating the graph as a **closed world**: if the
graph returned no test for a symbol, we printed "has no covering test" as a
fact; if it returned any edge from a test file, we printed it as proof the test
runs the code. Both are wrong the moment a repo uses interfaces.

Two things made this concrete rather than theoretical:

- **All 9 `TESTS` edges in our own repo are resolved by name, not by tracing.**
  Zero are `exact`. We were calling those "proven".
- **`impact` prints `Completeness: degraded for Go (...)` on every run, and
  `simulate` printed it zero times** on the identical snapshot. We had plumbed
  the diagnostics into our response object and never read them. So we were
  emitting a bare `RISK HIGH` off a snapshot the provider itself flags as
  incomplete.

**The fix, in one sentence:** evidence is now tiered by *how the provider
resolved the edge* rather than by the edge's existence, coverage has three
states instead of two, partial analysis is announced with its reasons, and
every claim we cannot prove ships with the command that settles it.

**The proof it is honest.** The fixture in
[`evidence/06-curveball-fixture.txt`](evidence/06-curveball-fixture.txt) is a
repo where `Gateway` has two implementations, so the interface call cannot be
resolved. `TestCheckoutWithStripe` genuinely executes `chargeViaStripe` — Go's
own coverage tool says **100% covered** — and the graph cannot see it. The old
output said "no covering test", which was **false**. The new output says no
test could be *resolved*, flags the analysis partial, and prints the command to
check.

**What did not change**, and is pinned by test: `1292 of 1405` covered symbols
in this repo have a properly resolved test edge. All of them still read
`edge` / `confirmed`, with the same counts and the same `RISK`. The defect
affected the other `113`.

---

## 10. Demo script — 60 seconds

1. *"Before you change code you want to know what breaks. This tool already told
   you that. What it never told you is whether any test would **catch** it."*
2. Run the command. Stay quiet while they read.
3. Point at `UNCOVERED`: *"These are the answer. Change this function and
   nothing catches you."*
4. Open one of those files. *"Every claim has a real file and line. Nothing here
   is a guess you have to trust."*
5. Point at the word `edge` and the `[package]` beside it: *"That means the
   graph traced the call. `edge-heuristic` would mean the edge was drawn by
   matching a name — which is what an interface call produces. We label which
   one, every time, and we print the provider's own word for how it resolved."*
6. **The curveball, and this is the strongest 20 seconds you have.** Run it on
   the fixture:
   ```sh
   ./entire-graph simulate --repo /tmp/dispatch-fixture --symbol chargeViaStripe
   ```
   Then say: *"This function is 100% covered — Go's own coverage tool says so.
   The graph cannot see it, because the only path runs through an interface
   with two implementations. The old version of our tool said 'no covering
   test', and that was a lie. This version says no test could be resolved,
   flags the analysis as partial, and gives you the command to check."*
   Then show `evidence/06-curveball-fixture.txt` with the coverage numbers.
7. Volunteer limitation #1 from §7 — *"confirmed means the test reaches the
   code, not that it asserts the behaviour you are changing."*

**Warm the cache first, and use `--head --cache-dir`.** This is the single
biggest thing that can go wrong live:

```sh
./entire-graph simulate --repo . --symbol buildImpactResponseFromReader \
    --head --cache-dir .cache/graph
```

Run that once. The first run is 22s; every run after is 0.6s. If you forget
`--head`, you will stand in front of the judges for 22 seconds of silence on
every command.

**Build the demo fixture first too** — it lives in the test file, so create the
scratch copy before you present:
```sh
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
  go test ./internal/cli/ -run TestSimulateOnUnresolvableDynamicDispatchFixture -count=1
```
That proves the fixture works. For the live demo, the copy at
`/tmp/dispatch-fixture` (recipe in `evidence/06-curveball-fixture.txt`) is what
you point the command at.

---

## 11. Submission facts

| Field | Value |
| --- | --- |
| Track | Track 2 — Build with Graph Intelligence |
| GitHub fork | `https://github.com/arenforge/entire-graph-deathcode` |
| Entire mirror | `entire://aws-ap-south-1.entire.io/gh/arenforge/entire-graph-deathcode` |
| Mirror ID | `01M1TM59AMZQ38B7TCC4BFP25A` |
| Branch | `buildathon/main` |
| `format_version` | 2 (bumped by the curveball: `evidence` gained a third value) |
| Final commit SHA | fill in at 2:40 PM |
| Deadline | **3:00 PM IST — submit by 2:50** |

---

## 12. Rules that are easy to break by accident

- Work only in `deathcode`. Never in the other two folders.
- Never commit secrets, tokens or personal data — not in code, not in commit
  messages, not in `BUILDATHON.md`.
- Do not invent numbers. If we cannot verify it, we say so.
- Every person presenting must understand the code they are presenting.
