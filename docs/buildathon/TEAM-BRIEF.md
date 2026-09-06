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
./entire-graph simulate --repo . --symbol buildImpactResponseFromReader
```

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
TESTS      6 of 12 affected symbols have a covering test
  internal/cli/linereader_ondisk_test.go:23 edge  buildImpactResponseOnDisk -> buildImpactResponseFromReader
```
Six of the twelve are watched by a test. Each line reads:
**where the test lives** → **evidence tier** → **test name** → **what it covers**.

```
BREAKING   1 affected symbol is outside internal/cli (0 uncovered)
  ProviderSnapshot               internal/sem/provider.go:417
```
This affected code lives in a **different folder** from the function you are
changing. These are the ones you would never notice while reading your own
diff — someone else's code depends on you.

```
UNCOVERED  6 affected symbols have no covering test
  runImpact                      internal/cli/impact.go:124
  ...
```
**This is the product.** Six affected things have no test watching them. Change
your function, break these, and nothing catches you. Every line is a real file
and line number you can open.

```
RISK  HIGH - 6 of 12 affected symbols have no covering test (>= 5 uncovered)
```
A three-level verdict: NONE / LOW / MEDIUM / HIGH. **Not a made-up score** —
you can recount it from the two numbers printed above it.

```
Note: TESTS is a heuristic relation in this provider. Open the
cited lines before gating a merge on this verdict.
```
We print our own limitation on every single run.

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
| `internal/cli/simulate.go` | **New.** The whole command (~490 lines, heavily commented) |
| `internal/cli/simulate_test.go` | **New.** 6 tests |
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

Run them:
```sh
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null go test ./internal/cli/ -run Simulate -count=1
```

---

## 7. Known limitations — say these before a judge finds them

1. **"Covered" means a test *reaches* the code, not that it *checks the
   behaviour* you are changing.** This is the big one. Do not let the demo imply
   the stronger claim.
2. **The graph has only 9 explicit `TESTS` edges** in this repo, against 22,642
   `CALLS` edges. So most `edge`-tier evidence is really "a call from inside a
   test file". That is still direct proof the test runs the code, and we print
   the relation type on every line — but say it before someone greps for it.
3. **`TESTS` is a heuristic relation type** in this provider
   (`internal/sem/provider.go:649`). The tool prints this warning itself.
4. **Verified on this repository and Go only.** The stem-matching logic is
   tested against TS/Python/Ruby naming, but we have not run it on a real
   project in those languages.
5. **The full test suite has not been run on this branch yet.** We have no green
   baseline for the pre-existing tests. Must be done before submission.
6. **Cold start is ~21 seconds.** Warm the cache before demoing.

---

## 8. What is left to do

| Who | Task |
| --- | --- |
| P2 | JSON contract test in `internal/cli/` (assert `format_version`, `affected_total`, `uncovered_total`, `tests_heuristic`) |
| P3 | Implementation order: topological sort of the affected subgraph so it says what to change first |
| P4 | Finish `BUILDATHON.md` — problem, architecture, graph findings, limitations from §7 |
| Lead | Full test suite at ~2:10 PM, final semantic diff evidence at ~2:30, submit by 2:50 |

**Full suite** (takes a while, start it and keep working):
```sh
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null go test -timeout 30m ./...
```

**Final evidence** (required by the rules):
```sh
./entire-graph diff --base 3a2a715 --head HEAD > docs/buildathon/evidence/05-final-semantic-diff.txt
```

---

## 9. The noon curveball — the process is graded, not just the code

At 12:00 the organisers hand out an extra mandatory constraint. Follow this
order exactly:

1. **End the current agent session before reading the card** (New Chat, or
   `/clear`). This is required and it also seals our checkpoint.
2. Read the card.
3. Start a fresh agent session in the `deathcode` folder.
4. **Before editing anything**, make the agent rebuild its understanding from
   our checkpoints:
   ```sh
   entire checkpoint list
   entire checkpoint explain <id>
   ```
   Our commit messages are written densely on purpose so this works.
5. Capture graph evidence **before** changing the affected area:
   ```sh
   ./entire-graph simulate --repo . --symbol <affected thing> > docs/buildathon/evidence/04-before-curveball.txt
   ```
6. Implement the **smallest complete** response. Test it. Commit with what
   changed and why.

---

## 10. Demo script — 60 seconds

1. *"Before you change code you want to know what breaks. This tool already told
   you that. What it never told you is whether any test would **catch** it."*
2. Run the command. Stay quiet while they read.
3. Point at `UNCOVERED`: *"These are the answer. Change this function and
   nothing catches you."*
4. Open one of those files. *"Every claim has a real file and line. Nothing here
   is a guess you have to trust."*
5. Point at the word `edge`: *"That means the graph proved the test runs this
   code. `mirror` would mean a weaker filename-based guess. We label which one,
   every time."*
6. Show the curveball commit and the test that proves it.
7. Volunteer limitation #1 from §7.

**Warm the cache first.** Run the demo command once before you present.

---

## 11. Submission facts

| Field | Value |
| --- | --- |
| Track | Track 2 — Build with Graph Intelligence |
| GitHub fork | `https://github.com/arenforge/entire-graph-deathcode` |
| Entire mirror | `entire://aws-ap-south-1.entire.io/gh/arenforge/entire-graph-deathcode` |
| Mirror ID | `01M1TM59AMZQ38B7TCC4BFP25A` |
| Branch | `buildathon/main` |
| Final commit SHA | fill in at 2:40 PM |
| Deadline | **3:00 PM IST — submit by 2:50** |

---

## 12. Rules that are easy to break by accident

- Work only in `deathcode`. Never in the other two folders.
- Never commit secrets, tokens or personal data — not in code, not in commit
  messages, not in `BUILDATHON.md`.
- Do not invent numbers. If we cannot verify it, we say so.
- Every person presenting must understand the code they are presenting.
