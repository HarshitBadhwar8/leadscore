# Carve: leadscore

Log for carving the private repo `HarshitBadhwar8/leadscore` into the public repo leadscore, per
the open-source-carve skill. This is S19's first step: the card. No carve stage has run yet.

This log lives here in leadscore, under `docs/design/`, which the carve drops, so it never ships.
It is not in core's open-source plan folder: by decision, nothing for this carve is committed to
core.

## Entry

Not entered. The card was added on 2026-10-08, with Harshit's authorization relayed by the
session (not a quoted sign-off). The carve will enter in mode A: export the private repo's tree
at a stated commit with `git archive`, then one scrubbed commit.

A dry run (rehearsal) ran on 2026-10-07 on a scratch copy of `main` at `3151f23`, without the
skill or the plan. Nothing was pushed, tagged or created on GitHub. Its findings feed "Before the
carve" below. The rehearsal log stays in the private repo.

Plan source (read only): core's plan branch `origin/worktree-open-source-inventory` (STATE.md is
not on master yet). The older carve logs live on that branch. This card is not in the plan's team
page; the "For the plan" notes below are for the plan's owners to copy if they want them.

## Inputs

| Input | Value | Status |
|---|---|---|
| Source | `HarshitBadhwar8/leadscore` (private), whole repo; `main` is at `6b0d660` on 2026-10-08, and the export commit is fixed at stage 2 | decided (card) |
| Excluded paths | `docs/design/` (the README and `SKILL.md` carry the public design); the CI job that pushes to the private image registry | decided (S19 brief) |
| Repo name | `leadscore` | working assumption; Rohan confirms (T2: names of unstaged repos are open) |
| Shape | Go CLI plus runnable service (docker compose); see shapes.md, both sections | decided (card) |
| Module path | `github.com/tetriz-ai/leadscore` | org decided (T2, `tetriz-ai`); name as above |
| Container image | `ghcr.io/tetriz-ai/leadscore` | decided (S19 brief); made public at publish |
| License | MIT | decided (O2) |
| Copyright holder | "Workloom Solutions Private Limited", as on every sibling repo (outbox-comms, ghattach, logredact) | in the repo since 2026-10-08 (LICENSE, NOTICE, source headers); O1 confirms |
| Maintainer | Rohan Chougule (@rchougule) | decided (T3); sign-off pending |
| Staging org | open: Harshit's GitHub account is not a member of `tetriz-ai` (membership API: 404) and is a member of `workloom-dev` (204), where the plan stages private rehearsal copies and Actions is blocked | open; Rohan decides (T2) |
| First version | v0.1.0 | default; S19 brief |
| Commit author | Harshit's GitHub noreply address until the release account exists | working assumption (T2: release account open) |
| Acceptance credential | none; the acceptance run is CSV only, with no vendor account | n/a |
| Target folder | `~/oss-carve/leadscore`; root `~/oss-carve` | working assumption |

Card:

> **What it does:** an outbound engine. It brings leads in (CSV files, Google Sheet tabs, Apollo
> webhooks), merges them into people, scores them with the user's own YAML rubric, routes each
> lead to one lane, and pushes it to Apollo sequences, HubSpot or an export list. It reads
> replies and opt-outs back, so nobody who replied or opted out gets cold outreach again.
> **How someone uses it:** install the `leadscore` binary (release binaries for macOS, Linux and
> Windows, or `go install`), or run the image with `docker compose`; then a rubric, a
> `leadscore.yml` and a CSV of leads.
> **Acceptance:** from a fresh clone, no vendor account, no credential: a CSV-only `docker
> compose` run on the shipped CSV-only example (config, rubric and leads) fills `./out` with the
> lane export lists. The rehearsal's plain-binary run of the same example gave 8 leads scored,
> `out/call-list.csv` with 5 rows and `out/nurture.csv` with 1, both mode 0600; the compose form
> has not run yet.

Card claims checked against the code: not yet. Stage 0 runs the shape's dependency lister on the
export and greps for each core coupling the credit table below names.

### Intended value

Not supplied. Harshit may answer the optional question at stage 0; otherwise it is inferred from
the card and the capability map before the README is drafted, and marked as an assumption.

### Copied code and history check

Files that came from core, with the authors from `git log --format=%an` (rename-following) on core
at `29c083292`. The list is for O1's author list, not for credit: credit is the team list
(`CONTRIBUTORS.md`, byte for byte), and every person below is already on it.

| Landed in | Core source | Authors |
|---|---|---|
| `internal/logredact` | `backend/pkg/logredact/redact.go`, `redact_test.go`, `vendor_error_test.go` | Aniket Ukharde, Rohan Chougule, Surya Gangaraj (@0xSG) |
| `internal/receiver/auth` | `backend/pkg/channelauth` and its test | Surya Gangaraj |
| `internal/merge/emailshape.go` and test, `emaildomain.go`, `normalize.go` | `backend/workloom/gtm/outreach/email_shape.go` and test, `backend/pkg/emaildomain/public.go`, `outreach/service.go` helpers and `derive_domain_test.go` | Surya Gangaraj |
| `internal/api/aliases.go` | `backend/scripts/god-script/processor/gtmleadscsvimport.go` alias map | Surya Gangaraj |
| `internal/rules/bands.go` | `backend/workloom/gtm/relevance/score.go` band lookup | Surya Gangaraj |
| `adapters/apollo` | `backend/workloom/gtm/outreach/apollo/{client,notification,notification_reserved_test,enrichment,enrichment_test,website_visit,service,oversize_test}.go`, `outreach/enrichment_mapping.go` | Surya Gangaraj |
| `adapters/hubspot` | `outreach/hubspot/{client,client_test,properties_test}.go` | Surya Gangaraj |
| whole repo | the private repo's own history | Harshit Badhwar |

Surya Gangaraj's git name in core is stylised; the team list's form is used. `internal/logredact`
and `internal/receiver/auth` are copies maintained here: leadscore is standalone and depends on no
other company repo. Their package comments say so; the copyright holder is the same, so NOTICE
needs no entry for them.

### Before the carve

Status on 2026-10-08:

- **Rehearsal:** done (2026-10-07). The scrubbed copy built, passed vet and `go test -race`, and
  the CSV-only path filled `./out`. Every forbidden-term grep came back clean.
- **Live vendor checks:** pending. The vendor fixtures on `main` are still marked `provisional`.
- **Staging repo:** not created; see the staging org row.
- **Release workflow:** `.github/workflows/release.yml` exists on `main`. The carve checks it
  against shapes.md's Go CLI and runnable-service rules (goreleaser config, dependency license
  texts under `third_party/licenses`, multi-arch image with OCI labels).

Open, each with who decides:

| Question | Who decides |
|---|---|
| Where leadscore stages, and the release account | Rohan (T2) |
| Repo name `leadscore` | Rohan (T2) |
| Ship before the live vendor checks land, with a README note on what is unconfirmed, or wait | Harshit |
| Dropping `docs/design/` leaves references to it in code comments, the README, `SKILL.md`, examples and test helpers: publish a scrubbed reference doc, fold it into the README, or strip the references | Harshit |
| Maintainer sign-off on the plan card and the carve | Rohan |

Skill rules the rehearsal did not follow, which the carve applies: NOTICE holds the copyright
line and third-party pieces only, not people (the rehearsal listed authors there);
`CONTRIBUTORS.md` is the skill's team list byte for byte; `examples/` becomes one folder per task
with a `run.sh`, an index and an `Examples` CI job; fixtures use reserved domains only.

Machine safety (`{{safety}}`), shared part: "Use the carve's shared caches: for Go,
`GOMODCACHE=~/oss-carve/cache/gomod`, `GOCACHE=~/oss-carve/cache/gobuild`,
`GOPROXY=https://proxy.golang.org`, with `GOPRIVATE`, `GONOPROXY` and `GONOSUMDB` empty. Never
the machine's default cache, and never a private one of your own. Delete your temp clone and
scratch copies when you finish, unless your brief says the coordinator reads them." The code
reads vendor and Google credentials: never read a keychain, cookie store or credential file;
never call Apollo, HubSpot or Google live; run with fakes under a temporary HOME. The compose run
needs one container per recipient; the per-recipient lines are set at stage 0.

Preflight: not run yet.

## Pre-read

Not started.

## Export

Not started.

## Scrub

Not started.

## License and third-party

Not started.

## Contributors

Not started.

## Stand-alone changes

Not started.

## Presentation

Not started.

## Review panel

### Pass 1

Not started.

### Pass 2

Not started.

### Reproductions

Not started.

### Re-check

Not started.

### README read

Not started.

## Sign-offs and approvals

None recorded.

## Deviations

- **Source outside core.** The skill exports from the core repo. leadscore's source is a separate
  private repo, so stage 2 runs `git archive` there, at a stated commit, instead of on core
  source paths. The read-only rule applies to that repo as it would to core.

## Push-day replacements

Added with the open-source repo files (PR #33). Each is one text replacement on publish day.

| Placeholder | Becomes | Where |
|---|---|---|
| `HarshitBadhwar8/leadscore` | the public repo path (org per T2) | `go.mod` (module path) and every Go import; `.github/ISSUE_TEMPLATE/config.yml:4`; `CHANGELOG.md:19-20`; `.golangci.yml:23,51`; `scripts/third-party-licenses.sh:18,52` |
| `RELEASE_DATE_PLACEHOLDER` | the day the v0.1.0 tag is cut | `CHANGELOG.md:9` |

## Staging

Not started.

## For the plan

- Add a leadscore card to the team page, section 1.1, from the card above.
- The skill assumes the source is in core; a carve from a separate private repo has no stated
  mode. This carve uses mode A with `git archive` on that repo.

## Time taken

Not started.
