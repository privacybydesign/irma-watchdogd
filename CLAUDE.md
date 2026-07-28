# CLAUDE.md

`irma-watchdogd` is a single Go binary that runs a fixed set of checks against the
public IRMA/Yivi infrastructure on a timer and reports what it finds over HTTP,
webhooks and Slack. `README.md` describes what it checks and how to install it;
`config.yaml.example` is the reference for every configuration option. This file
covers the things you need to know before changing the code.

## Source files

| File | What lives there |
| --- | --- |
| `main.go` | Config struct, startup, the check cycle (`runChecks`), the debounce (`confirmIssues`), the HTTP handler, the Slack and webhook senders, and all `check*` functions except the HTTP health checks. |
| `health_check.go` | The `HealthCheck` config type and `runHealthChecks`, the configurable request/response check. |
| `timing.go` | `requestTrace`: per-attempt DNS/connect/TLS/first-byte timings via `net/http/httptrace`, plus `logFailedAttempt`. |
| `util.go` | `newHTTPClient` (the shared retrying client) and the log-safety helpers `truncateForLog`, `redactURL`, `redactErr`, `redactErrs`. |
| `issue_entry.go` | `issueEntry` (a `warning`/`danger` type plus a message string) and the `issueEntries` slice helpers. |
| `scripts/probe.sh` | Standalone probe that reproduces the checks in a loop with per-phase timing, for failures that only happen from the watchdog's vantage point. Its header has the exact `kubectl` invocations. |

Tests: `main_test.go` (webhook delivery and the debounce), `webhook_test.go`,
`util_test.go`, `timing_test.go`, `race_test.go`.

## The check cycle

`main` parses `config.yaml`, installs the configured schemes into a temporary
`irma_configuration` directory, registers the HTTP handler, and starts one
goroutine that calls `runChecks` and then waits on a `time.Ticker` of
`conf.Interval`. There is one interval for the whole program. Checks do not have
their own schedules, and they run one after another inside that single goroutine.

`runChecks` does the following, in order:

1. Increments `cycleCount` and sets `initialCheck` (true for the first
   `FailureThreshold` cycles).
2. Calls each check and concatenates the returned `issueEntries`:
   `checkSchemeManagers`, `checkCertificateExpiry`, `checkAtumServers`,
   `runHealthChecks`.
3. Logs the raw findings.
4. Passes them through `confirmIssues`, which returns the confirmed set.
5. Diffs the confirmed set against the previously published one (`difference`)
   to get the new and fixed entries.
6. Sends those to Slack and the webhooks.
7. Publishes the confirmed set with `setState`.

A check reports a finding by returning an `issueEntry`. It never talks to Slack
or the webhooks itself.

### Where findings end up

* **HTTP GET** on `bindaddr`: `handler` reads `currentState()` and renders the
  full confirmed set, warnings and dangers alike. This is the pull view, so it
  shows what is wrong right now rather than what changed.
* **Webhooks**: `pushToWebHooks` receives only the *new* entries and filters them
  down to `danger`. Warnings never reach a webhook. Delivery is skipped entirely
  while `initialCheck` is true, so a restart does not replay known problems.
* **Slack**: `pushToSlack` receives the new and fixed entries. Dangers get a
  `<!channel>` mention, warnings a message without one, fixed entries a green
  one. Slack is not suppressed on the initial check; it posts an "I just
  (re)started" note first instead.

The configured webhook URL is a template with a literal `%s` in it. It is
substituted with `strings.Replace`, not used as a format string, on purpose.

## Adding a check

Follow `checkAtumServers` for the simplest example.

1. **Config field:** add it to `Conf` in `main.go`. There are no yaml tags, so
   `gopkg.in/yaml.v3` maps the lowercased field name: `CheckAtumServers` reads
   the key `checkatumservers`. There is no per-check interval option to add;
   everything runs on `interval`.
2. **The function:** put it in `main.go` next to the other `check*` functions.
   Signature is `func checkX() (ret issueEntries)`, or take
   `*irma.Configuration` if you need the scheme configuration, as
   `checkSchemeManagers` does. Only add a new file if the check brings its own
   config type, which is why `health_check.go` is separate.
3. **HTTP:** use `newHTTPClient()` from `util.go` rather than `http.Get`; it
   retries, which is what keeps a single dropped packet from paging anyone.
   Attach `newRequestTrace()` and call `logFailedAttempt` from `CheckRetry` the
   way `checkCertificateExpiryOf` does, so a failure records which phase hung.
4. **Wire it up:** add one line to `runChecks`:
   `curIssues = append(curIssues, checkX()...)`. That is what gets the check
   debouncing and all three output paths.
5. **Severity:** `danger` reaches webhooks and mentions `<!channel>`; `warning`
   only shows up on the HTTP page and in a quiet Slack message. Scheme problems
   are warnings because the app keeps working without the scheme.
6. **Message text is the debounce key:** `confirmIssues` counts streaks per
   message string, so the message must be identical on every cycle the problem
   persists. Do not interpolate a timestamp, a duration, an attempt count or a
   random port into it. A message that varies never reaches its streak
   threshold, so it is never reported at all, and if the threshold is 1 it is
   reported as new every cycle instead.
7. **Logging:** never log a webhook or Slack URL directly, and never log a
   response body directly. Use `redactURL`/`redactErr` and `truncateForLog`.
   Webhook URLs carry a secret in the path, and errors from `net/http` embed the
   full URL in their message.
8. **Config example:** add the new option to `config.yaml.example`.
9. **Test it:** see below.

## Debouncing

`confirmIssues` in `main.go` sits between the checks and every output path. It is
symmetric: an issue must be present for `FailureThreshold` consecutive cycles
before it is confirmed, and absent for that many before it is dropped and
reported fixed. `FailureThreshold` comes from `failurethreshold` in the config,
defaults to 3, and is floored to 1 (1 restores alert-on-first-cycle behaviour).
Duplicate messages within one cycle are collapsed so they cannot advance a streak
twice.

The state is three package-level maps in `main.go`: `failureStreaks`,
`recoveryStreaks` and `confirmedSet`. `initialCheck` stays true for the first
`FailureThreshold` cycles, which is exactly long enough to cover the delay before
a startup outage can be confirmed, so a restart is never mistaken for a new
problem on the webhook path.

Anything that returns entries from `runChecks` is debounced. A check that pushed
to Slack or a webhook itself would bypass all of this, which is the main reason
checks only return values.

## Shared state

`issues` and `lastCheck` are written by the check goroutine and read by the HTTP
handler. Go through `setState` and `currentState` only; both take `stateMu`.
Touching the globals directly is a data race that can crash the server on a
torn read. `race_test.go` hammers the handler and `setState` from several
goroutines to cover this, but it can only catch a regression under `-race`, so
plain `go test ./...` will pass even if the locking is removed.

## Tests

The tests run against `httptest` servers, so they need no config file and reach
nothing on the internet once the modules are downloaded.

```
go build ./...
go vet ./...
gofmt -l .
go test ./...
go test -race ./...
go test -race -run TestHandlerStateRace ./...
```

Two things to know:

* `go.mod` pins Go 1.26.3, and the toolchain plus the `irmago` dependency tree
  are fetched on the first run. Expect the first `go test ./...` to take about
  20 seconds and later ones under a second.
* The debounce tests share the package-level debounce state. Call
  `resetDebounceState(threshold)` (in `main_test.go`) at the top of any test that
  runs a cycle, or it will inherit streaks from whichever test ran before it.

CI (`.github/workflows/status-check.yaml`) only runs the Dockerfile build stage.
It does not run the tests, so run them locally before pushing.

## Diagnostics

When the watchdog reports a host as unreachable but the host looks fine from a
browser, the failure is usually specific to where the watchdog runs. Use
`scripts/probe.sh` from inside the cluster and from a workstation at the same
time and compare the per-phase timings. The Diagnostics section of `README.md`
explains the comparison.
