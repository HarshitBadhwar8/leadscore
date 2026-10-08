# leadscore

## What this is

An outbound engine: it brings leads in (CSV, Google Sheet tabs, Apollo webhooks), merges them
into people, scores them with a YAML rubric, routes each to one lane, and pushes it to Apollo,
HubSpot or an export list. It writes no emails, and it never contacts a person who opted out.

## Layout

- Module root (package `leadscore`): the public API. `cmd/leadscore/` is the binary.
- `internal/api` holds the public types; the root aliases them. `internal/engine` is the run.
- `internal/store/{sqlite,sheets}` are the built-in stores. `adapters/` holds sources and sinks.
- `internal/receiver` is the webhook receiver. `internal/fakes` fakes every vendor for tests.
- `storetest/` and `sinktest/` are the conformance suites for plug-in stores and sinks.
- `examples/` holds made-up rubrics, configs and leads. `readme_test.go` loads them.
- Tests sit beside the code. Root `*_test.go` files hold repo-wide rules (CI, docs, imports).
- `SKILL.md` is for agents running an install, not for changing this repo.

## Commands

Go 1.25 is the floor. CI tests 1.25, 1.26 and 1.27, and runs every other check on 1.27.x.

```sh
go build ./... && go vet ./... && go test -race ./...
GOOS=windows go build ./... && GOOS=windows go vet ./...
```

The other checks CI runs, each as CONTRIBUTING.md lists it:

| Check | Command |
|---|---|
| Format | `gofmt -l .` prints nothing |
| Lint | `golangci-lint run`, from `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0` |
| Shell | `shellcheck setup/gcp.sh scripts/*.sh testdata/gcloud/gcloud testdata/compose/smoke.sh`, shellcheck 0.11.0 |
| Dockerfile | `hadolint --ignore DL3018 --ignore DL3066 Dockerfile` |
| Vulnerabilities | `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` on Go 1.27.x |
| Licenses | `go install github.com/google/go-licenses/v2@v2.0.1`, then `scripts/third-party-licenses.sh --check` |
| Docker | `docker build -t leadscore:ci .`, then `testdata/compose/smoke.sh leadscore:ci` |

Live checks against real services skip unless their `LEADSCORE_LIVE_*` variable is set
(CONTRIBUTING.md, "Live checks"). CI never sets them.

## Contract to keep

- An opted-out person is never pushed: `internal/e2e/suite_test.go` (`TestOptOutsWithVendors`,
  `TestOptOutsCSVOnly`) and `outcomes_test.go`.
- No person gets a second cold push: `TestNoSecondColdPushAfterALaneChange` in
  `internal/e2e/suite_test.go`.
- A webhook is stored before it is answered: `TestNoAnswerBeforeTheEventIsStored` in
  `internal/receiver/handler_test.go`.
- Logs carry ids, never emails or secrets: `internal/logredact/redact_test.go`, and
  `TestErrorsRedactedInsideCloudRun` in `internal/cli/cli_test.go`.
- The public API is frozen at v0.1.0: interfaces never gain methods. `stub_adapters_test.go`
  compiles a stub of every kind against it.
- Only `cmd/` and `_test` files import the root package: `imports_test.go`.
- `Run` returns an error when it cannot start: `TestRunFailsWithoutConfig` in
  `stub_adapters_test.go`. Problems found during a run go to `RunResult.Problems` and Health.

## Making a change

- Write the test first: a behaviour change needs a test that fails before and passes after.
- Tests use the fakes in `internal/fakes`, never a real key.
- Add a line under `Unreleased` in `CHANGELOG.md` for anything a user would notice.
- Never silence a lint finding inline. Fix it, or change `.golangci.yml` with a comment.
- A new plug-in follows CONTRIBUTING.md, "Adding an adapter or a store".
- After a dependency change, run `scripts/third-party-licenses.sh` and update `NOTICE`.
- A change to `docs/reference.md` tables or `SKILL.md` keeps `docs_test.go` and `skill_test.go` green.

## Do not

- Do not import the root package from `internal/`, `adapters/`, `storetest` or `sinktest`.
- Do not point a test at real Apollo, HubSpot or Google. Live checks stay behind their variables.
- Do not log an email address. Pass log text through `internal/logredact`.
- Do not edit `third_party/licenses/` by hand. The script writes it.
