# CLAUDE.md: rules for AI agents (and humans) working on tasmofleet

Read this file first, then the parts of the spec you need
(`docs/spec/2026-10-18-tasmofleet-design.md`). Human contributors: see also
[CONTRIBUTING.md](CONTRIBUTING.md).

## Ground rules

- **English only** in everything committed: code, comments, docs, commit messages, test names.
- **Test first.** Write the failing test, then the code. Tests say _why_ the behaviour
  matters (a comment or the test name), not just what it does. A test that cannot fail
  when the business rule changes is wrong.
- **No network in unit tests.** Only loopback listeners the test itself starts
  (`httptest`, the simulator, the embedded MQTT broker, the mock OIDC provider). Nothing
  reaches the internet, a real broker or a real device. Integration tests (build tag
  `integration`) follow the same rule.
- **Stay inside your change's scope.** You may read everything, but change only the
  packages your change is about. If a contract (interface or type
  in `internal/model` or another package's exported API) has to change, stop and
  report it instead of editing it. Contract changes go through the spec.
- **Minimal dependencies.** The allowed modules are listed in the spec ("Dependencies").
  Adding one needs a written justification in the spec in the same change.
- **Fail loud.** Never report "done" or "tests pass" if anything was skipped. No
  `t.Skip` without an issue reference. No ignored errors (`_ =` only with a comment
  saying why).
- **Match conventions.** gofmt, `log/slog` for logging, `errors.Is/As`, context on every
  I/O call, table tests, `t.TempDir()` for files.

## No backward-compatibility shims

tasmofleet has no stable external API before 1.0 (maintainer's rule).
Don't add or keep compatibility layers: no redirects for moved routes, no deprecated
config/env aliases, no old CLI command names, no API compat layers. Breaking changes are
fine. Record each one in `CHANGELOG.md` (section "Unreleased", then the release).
**The only exception is user data:** migrations stay append-only, and the live database
must upgrade cleanly.

## Product principles (details in spec §1.1a)

- **Minimal mandatory configuration**: environment = bootstrap only. Every other setting
  (SSO included) is live in the UI and stored in the database; env values lock their
  field. New settings get a UI/DB setting, not a new environment variable.
- **Proportionate security (home-lab tool)**: keep the basics (CSRF, authz, no secrets in
  logs/HTML, chip-family guard, secrets only to their bound target). Step-up only for
  local account management and auth/SSO changes (not for restore, 2026-10-20). Reviews: fix High/Medium,
  document Low/Info as residual risk, no review loops.

## Architecture map

```
cmd/tasmofleet            CLI: serve, user, sessions, import, healthcheck, version
cmd/tasmofleet-devenv     simulator fleet + embedded MQTT broker + mock OIDC, for e2e/dev
internal/model            plain domain types (leaf package, stdlib only)
internal/config           env + file configuration (TASMOFLEET_*)
internal/store            SQLite (modernc.org/sqlite), embedded migrations, repositories; the ONLY SQL
internal/secretbox        AES-GCM for device passwords at rest
internal/clock            time abstraction for testable timeouts
internal/tasmota          device HTTP API client + response parsers (SSRF-guarded)
internal/fwselect         PURE: identity detection, artifact selection, image header check, safety guard, plan
internal/firmware         upstream sources, firmware cache, device-facing firmware HTTP server
internal/ota              PURE state machine (machine.go) + engine (engine.go)
internal/mqttbus          optional MQTT connection (paho), topic decoding
internal/health           health tracker (MQTT + HTTP poll fallback)
internal/events           in-process pub/sub for SSE
internal/inventory        device service layer (validation, upsert, audit)
internal/tasmoadmin       TasmoAdmin devices.csv / MyConfig.json parsers
internal/backup           .dmp + snapshot + decoded JSON backups, retention, Decoder
internal/audit            audit recorder
internal/auth             OIDC, trusted headers, local break-glass, sessions, roles
internal/httpx            proxy/base-path/public-URL handling, security headers, CSRF
internal/web              templ + HTMX UI, SSE endpoint
internal/web/ui           templ components (page shell, design system components)
internal/metrics          Prometheus text exposition (no client_golang)
internal/app              wiring and lifecycle only
internal/simulator        Tasmota device simulator (tests and devenv only)
internal/testutil/oidcmock mock OIDC provider (tests and devenv only)
internal/discovery, bulk, policy, notify   M2 features
internal/deps            build-tagged file pinning third-party modules in go.mod (maintainer only)
```

## Package boundaries (enforced by depguard in `.golangci.yml`)

- `internal/model` imports only the standard library.
- `internal/fwselect` and `internal/ota/machine*.go` are pure: no `net`, `os`, `database/sql`.
  They take data and return decisions. These are the safety-critical parts and run
  under mutation testing.
- Only `internal/store` imports `database/sql` / `modernc.org/sqlite`.
- Production code never imports `internal/simulator`, `internal/testutil/...` or the
  embedded MQTT broker.
- `github.com/a-h/templ` is imported only by `internal/web` and `internal/web/ui`. `templ.Raw`,
  script and css components, `style` attributes and inline handlers are forbidden (CSP;
  `forbidigo`, a source test in `internal/web/ui`).
- Consumers depend on interfaces (`store.DeviceStore`, `tasmota.Client`, `ota.DeviceAPI`, ...),
  never on concrete types from another package. Fakes live in the consumer's `_test.go`.
- Device network access goes only through `tasmota.Client` with a `tasmota.Target`
  built from an inventory device. Firmware downloads go only through `firmware`
  sources. Nothing else makes outbound HTTP requests (OIDC discovery in `auth` is the
  one configured exception).

## Generated code: templ

`*.templ` files are the source; the `*_templ.go` next to them are generated and committed (so
`go build` and the Dockerfile need no generator). Never edit a `*_templ.go`. After changing a
`.templ`, run `make templ` and commit both. `make templ-check` (part of `make check` and the CI
lint job) regenerates and fails when that changed a file. The generator is pinned in
`tools/go.mod` and its version must equal the `github.com/a-h/templ` version in `go.mod`
(bump both together). Every page renders inside `ui.Shell`; there are no html/template pages.

## Safety rules that must never regress

1. `fwselect.Guard` runs immediately before every `OtaUrl` and `Upgrade`. It refuses on
   unknown hardware, chip-family mismatch (device vs artifact header vs file name), safeboot
   images, and variant changes without explicit user choice. Never bypass it, never "guess".
2. `Status 0` is never sent by automated code (heap bursts corrupt ESP8266 replies). Use
   individual `Status N` queries, one at a time per device.
3. `Upgrade 1` is never retried blindly. After a lost response the engine probes first.
4. Device credentials never reach the browser, logs, errors or audit details.
5. Local accounts are managed by the CLI or by admins in the UI (`/users`) with step-up: the
   current password for local admins, the IdP re-authentication for OIDC admins. Header
   principals have no step-up and only view. Operators can never create accounts or raise
   roles. Listing needs no step-up, every change does.
6. Forwarded headers are trusted only from `TRUSTED_PROXIES` (checked on `RemoteAddr`).

## The Tasmota simulator

`internal/simulator` replays real captures from `testdata/fixtures/devices/` and models
the OTA state machine documented in `docs/research/tasmota-minimal-and-ota.md`
(pending, downloading, reboot gap, minimal firmware with its 5 commands, settings not
persisted in minimal). Use it for every test that would otherwise need a device:

```go
fleet, _ := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
dev, _ := fleet.Add(ctx, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M, Version: "15.6.0"})
// dev.Addr() is a loopback host:port; dev.Commands() records what was sent
```

Fault injection (`simulator.Fault`) covers timeouts, dropped connections, unknown
commands, auth failures, failed downloads, long reboots and wrong-family firmware. Add a
fixture or a fault instead of mocking HTTP by hand.

## Running the gates

| Gate                                      | Command                                                                       |
| ----------------------------------------- | ----------------------------------------------------------------------------- |
| everything fast (run before every commit) | `make check`                                                                  |
| format                                    | `make fmt` / `make fmt-check`                                                 |
| go vet                                    | `make vet`                                                                    |
| staticcheck                               | `make staticcheck`                                                            |
| golangci-lint (incl. depguard boundaries) | `make lint`                                                                   |
| unit tests                                | `make test`                                                                   |
| race detector                             | `make race`                                                                   |
| integration tests                         | `make integration`                                                            |
| mutation tests (MUTATION in the Makefile) | `make mutation`                                                               |
| e2e (Playwright)                          | `make e2e-deps` once, `npx playwright install chromium` once, then `make e2e` |
| vulnerabilities                           | `make vuln`                                                                   |
| everything CI runs                        | `make ci`                                                                     |

Tools are pinned in `tools/go.mod` and run via `go tool -modfile=tools/go.mod <tool>`.
Run a single package's tests with `go test ./internal/fwselect/ -run TestGuard -v`.
Parallelism follows the machine: `JOBS` defaults to `nproc` (`GOFLAGS=-p=$(JOBS)`),
mutation workers to `nproc/2`, Playwright workers to `E2E_WORKERS` (default 6). Locally the Makefile runs tools under `nice -n 15`; CI sets `NICE=` (empty). Override
with e.g. `make check JOBS=4`. `make mutation-changed BASE=<ref>` runs the mutation gate
only for packages changed since `<ref>` (CI on pushes and PRs; tags and the nightly run
use the full `make mutation`). Docs-only changes skip CI and run the link check
(`tools/check-doc-links.py`) instead; see the path list in `.github/workflows/ci.yml`.

## Commits

- Commits are signed; `main` requires signed commits (branch protection). **Never** disable signing
  (`--no-gpg-sign`, `-c commit.gpgsign=false`). If signing fails, stop and report it.
- Write the message with a quoted heredoc, never `-m "..."` with backticks or `$(...)`:

  ```sh
  git commit -F - <<'EOF'
  fwselect: refuse safeboot images in Guard

  Body in English: what and why.

  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  EOF
  ```

- Trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` on AI-authored commits.
  No session links or other URLs to chat sessions.
- Commit only your own files. `make check` must pass before each commit.
- Repository: `https://github.com/nofuturekid/tasmofleet` (HTTPS remotes).
- No secrets in the repository, not even in tests: use obviously fake values.
- **Migrations are append-only since tag v0.0.1.** Never edit a merged file in
  `internal/store/migrations/`; add `NNNN_name.sql`. (0001 was amended before the first tag;
  dev/test databases created before that must be recreated.)

## Releases

- **SemVer** from v0.1.0: new features or visible behaviour -> minor (`0.x.0`); bug fixes
  and small changes only -> patch (`0.x.y`). Breaking changes are allowed before 1.0 but
  must be listed in `CHANGELOG.md`.
- `CHANGELOG.md` has one section per version with the subsections Added, Changed, Fixed,
  Breaking and Security (empty ones omitted). Every merge adds its lines under
  "Unreleased". The release PR moves them into a new "## X.Y.Z (date)" section BEFORE
  the tag is set; the tag build fails when that section is missing
  (`make changelog-check TAG=vX.Y.Z`).
- Tags are signed (`git tag -s vX.Y.Z`) and only set on a commit whose full CI
  (gates, integration, mutation, e2e, image) is green. The tag build publishes the image.

## Pull requests and the protected main

`main` is protected on GitHub: no force pushes, no deletion. Every change, docs-only
ones included, goes through a pull request: branch, push, open a PR against `main`, wait
until CI is green on the PR, then the maintainer merges it with `gh pr merge --squash`
(the only merge method enabled: GitHub signs squash commits, while its rebase merges would drop
the signatures that `main` requires). History stays linear; the branch must be up to date with
`main` before merging. Requirements: the status check `required` from `ci.yml`, linear
history, signed commits; admins are not exempt.
`ci.yml` runs on every PR: a `changes` job detects docs-only PRs and skips the heavy jobs
for them, and the final `required` job passes only when every heavy job passed (or was
skipped for a docs-only PR). `docs.yml` (link check) reports its own, non-required
status. No approvals are required (single maintainer). Release tags are still pushed
directly, on a green commit of `main`.

## Branches and worktrees

After a pull request is merged into `main`, its branch is deleted on origin and the
local worktree and branch are removed. Before deleting, check containment:
`git cherry origin/main <branch>` shows only `-` lines, or the branch's diff against
`main` is empty (rebased or squashed work). Branches with uncontained changes are never
deleted silently.

## Where things are written down

- Spec (what and why): `docs/spec/`
- Roadmap: `ROADMAP.md`
- Research (device behaviour, TasmoAdmin lessons): `docs/research/`
- Fixtures (real, sanitised device captures): `testdata/fixtures/` (see its README)
