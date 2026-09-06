# graph review — impact-aware change review for agent-written code

## One-sentence summary

`entire graph review` turns a commit range into a reviewable change-risk
verdict: every changed symbol, its blast radius, the tests that actually cover
it, and an explicit list of changes that nothing covers — each claim carrying a
`file:line` citation the reviewer can open.

## Problem, intended user and why it matters

The user is a developer reviewing a change an agent wrote. A Git diff answers
"what text moved". It does not answer the question that decides whether the
change is safe to merge: *what else depends on this, and is any of it tested?*
Today that requires running `entire graph impact` once per changed symbol and
correlating the results by hand. On an agent-authored change touching a dozen
symbols, nobody does that, so untested blast radius ships.

## Selected Entire track and why Entire is essential

**Track 2 — Build with Graph Intelligence.**

The product cannot exist without the graph. Mapping a changed line to the
symbol that contains it, that symbol to its transitive callers, and those
callers to the test functions that reach them is a call-graph query, not a text
query. Entire Graph supplies it locally, with no model calls, and returns
`file:line` for every node — which is what makes the verdict auditable instead
of advisory.

## Architecture and main workflow

```
commit range (--base/--head, or --checkpoint <id>)
      │
      ▼  existing diff path: changed entities per file
internal/sem  (semantic diff)
      │
      ▼  existing impact path: callers / callees / type consumers
internal/sem/dependents.go
      │
      ▼  NEW: correlate blast radius against test-reaching symbols
internal/cli/review.go
      │
      ▼
risk verdict + evidence table (text | json)
```

- Command registration: `internal/cli/root.go`
- New command implementation: `internal/cli/review.go`
- Coverage correlation: `internal/sem/`
- Evidence artifacts: `docs/buildathon/evidence/`

## Entire Graph findings and verification

Captured runs live in [`docs/buildathon/evidence/`](docs/buildathon/evidence/).

| # | Requirement | Artifact |
| --- | --- | --- |
| 1 | Graph search / definition lookup | `01-graph-search.txt` |
| 2 | Impact analysis before a high-risk change | _TODO before editing `sem`_ |
| 3 | Final semantic diff of the submitted implementation | _TODO at ~2:30 PM_ |

Every finding below was checked against source before we relied on it.

_TODO: record each finding, the file:line it pointed at, and what we verified._

## Noon Curveball: what changed and how we adapted

_TODO after 12:00._

## Checkpoint links and what each checkpoint proves

| Milestone | Checkpoint | What it proves |
| --- | --- | --- |
| Initial understanding and intended architecture | _TODO_ | |
| Last stable state before the Curveball | _TODO_ | |
| Response to the Curveball | _TODO_ | |
| Final implementation and verification | _TODO_ | |

## Setup, run and test instructions

```sh
git clone entire://aws-ap-south-1.entire.io/gh/arenforge/entire-graph-deathcode
cd entire-graph-deathcode
go build -o entire-graph ./cmd/entire-graph

# the new command
./entire-graph review --repo . --base HEAD~1 --head HEAD

# tests (the env vars keep fixtures from inheriting global git config)
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null go test ./internal/cli/ -run Review
```

Toolchain: Go 1.26, Git >= 2.36, Entire CLI >= 0.10.0, Entire Graph v0.4.0.

## Databricks use, data sources and limitations

Not applicable — this team did not opt in to the Databricks award category.

## Known limitations and next steps

_TODO: state plainly what is heuristic, which languages the coverage
correlation actually supports, and what we did not verify._
