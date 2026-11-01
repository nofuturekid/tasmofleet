# Every quality gate has a target. CI runs exactly these targets (tiers: CLAUDE.md "Running the gates").
#   make check     FAST tier, run before every commit (templ, fmt, vet, staticcheck, lint, unit and
#                  integration tests from the go test cache, vuln, build)
#   make e2e-core  FAST tier: the Playwright tests tagged @core
#   make safety    FAST tier when a safety-critical package changed (race + mutation of the changed ones)
#   make ci        FULL tier: check + race + mutation + all e2e (nightly, release tags)
#
# Tools are pinned in tools/go.mod and run with `go tool -modfile=tools/go.mod`,
# so no global installs are needed (first run builds them, then they are cached).

GO        ?= go
TOOL      := $(GO) tool -modfile=tools/go.mod
# Parallelism follows the machine; override with JOBS=n. A CI runner that
# runs several jobs at once sets JOB_SIZE_DIVISOR: each job then gets
# nproc/JOB_SIZE_DIVISOR (at least 2), and GOMAXPROCS of test binaries and the
# mutation workers follow JOBS. NICE keeps a desktop
# responsive; CI sets NICE= (empty).
JOBS      ?= $(shell n=$$(nproc 2>/dev/null || echo 4); j=$$(( n / $${JOB_SIZE_DIVISOR:-1} )); [ $$j -lt 2 ] && j=2; echo $$j)
export JOBS
ifdef JOB_SIZE_DIVISOR
export GOMAXPROCS := $(JOBS)
endif
NICE      ?= nice -n 15
export GOFLAGS ?= -p=$(JOBS)
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
BIN       := bin

# Packages under mutation testing as <package>:<minimum efficacy %> (share of
# covered mutants a test kills). Each owning work package raises its own entry
# (targets: fwselect 90, ota 85) as the code lands. A package without tests
# fails the gate, so add the entry together with the tests.
MUTATION := ./internal/fwselect:90 ./internal/ota:85 ./internal/policy:85 ./internal/cmdpolicy:90 ./internal/mqttcmd:85 ./internal/transport:90 ./internal/tasmota:85

# Safety-critical packages (CLAUDE.md "Safety rules": guard, OTA engine, device client and
# credentials, accounts, trusted proxies) plus the mutation packages. A pull request that changes
# one of them runs their race tests and the mutation gate (make safety BASE=<ref>) in the FAST
# tier; ci.yml reads this list.
SAFETY := ./internal/fwselect ./internal/ota ./internal/tasmota ./internal/secretbox ./internal/auth ./internal/httpx ./internal/policy ./internal/cmdpolicy ./internal/mqttcmd ./internal/transport

# go test flags for test and integration. Empty locally and on PRs, so unchanged packages come
# from the go test cache; the FULL tier passes TESTFLAGS=-count=1.
TESTFLAGS ?=
# Extra Playwright arguments, e.g. E2E_ARGS=--shard=1/2 (CI).
E2E_ARGS  ?=

.PHONY: catalogue catalogue-src catalogue-check docs-anchors docs-anchors-check demo demo-media check ci templ templ-check fmt fmt-check vet staticcheck lint test scripts-test shards-check race integration mutation mutation-changed mutation-sum mutation-plan safety safety-race test-quarantine e2e e2e-core e2e-quarantine e2e-deps visual visual-parity styleguide-docs vuln build image changelog-check changelog-title migration-guard changelog-notes clean tools

check: templ-check fmt-check vet staticcheck lint test scripts-test shards-check integration vuln build

# Unit tests of the registry cleanup's version selection (pure, no registry access).
scripts-test:
	node --test scripts/

# CI splits the FULL tier's race run (tools/race.sh, RACE_SHARD=i/2) and mutation run
# (MUTATION_SHARD=i/MUTATION_CI_SHARDS, tools/mutation-shard.sh) into shards. Fails when a package
# is in no race shard, or a file of a MUTATION package is dealt to no (or two) mutation shards.
# "make mutation-plan" prints the plan with the estimated load per shard.
MUTATION_CI_SHARDS := 4
MUTATION_PKGS := $(foreach e,$(MUTATION),$(firstword $(subst :, ,$(e))))
export MUTATION_PKGS
shards-check:
	@tools/race.sh --check
	@for p in $(MUTATION_PKGS); do \
	  want=$$(go list -f '{{range .GoFiles}}{{.}}{{"\n"}}{{end}}' "$$p" | sort); got=""; \
	  i=1; while [ $$i -le $(MUTATION_CI_SHARDS) ]; do got="$$got$$(tools/mutation-shard.sh $$i/$(MUTATION_CI_SHARDS) "$${p#./}" | awk '{print $$2}')\n"; i=$$((i+1)); done; \
	  got=$$(printf "$$got" | grep . | sort); \
	  [ "$$want" = "$$got" ] || { echo "shards-check: $$p files are not in exactly one of $(MUTATION_CI_SHARDS) mutation shards" >&2; exit 1; }; \
	done; echo "shards-check: every MUTATION package file is in exactly one of $(MUTATION_CI_SHARDS) mutation shards"

mutation-plan:
	@tools/mutation-shard.sh plan $(MUTATION_CI_SHARDS)

ci: check race mutation e2e catalogue-check

# templ components (internal/web/ui, internal/web) are committed together with their generated
# *_templ.go, so go build and the Dockerfile need no generator. `make templ` regenerates them;
# `make templ-check` (part of check, so of CI) regenerates and fails when that changed a file, i.e.
# when a .templ was edited without regenerating.
templ:
	$(NICE) $(TOOL) templ generate -path internal

templ-check:
	@sum() { find internal -name '*_templ.go' | LC_ALL=C sort | xargs -r sha256sum; }; \
	before="$$(sum)"; $(NICE) $(TOOL) templ generate -path internal >/dev/null || exit 1; \
	if [ "$$before" != "$$(sum)" ]; then echo "templ-check: generated files were stale and are regenerated now; commit them" >&2; exit 1; fi; \
	for t in $$(find internal -name '*.templ'); do [ -f "$${t%.templ}_templ.go" ] || { echo "templ-check: $$t has no generated file" >&2; exit 1; }; done

fmt:
	gofmt -w cmd internal

fmt-check:
	@out="$$(gofmt -l cmd internal tools)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(NICE) $(GO) vet ./...

staticcheck:
	$(NICE) $(TOOL) staticcheck ./...

lint:
	$(NICE) $(TOOL) golangci-lint run ./...

# Unit tests: no network except loopback listeners started by the test itself.
test:
	$(NICE) $(GO) test $(TESTFLAGS) ./...

# Every unit test with the race detector (FULL tier); tools/race.sh runs internal/auth in AUTH_PARTS
# parallel parts (default 2) next to the other packages.
race:
	$(NICE) tools/race.sh

# Race tests of the SAFETY packages changed since BASE (all of them without a usable BASE).
# tools/race.sh splits internal/auth (AUTH_PARTS) to keep the wall clock short.
safety-race:
	@if [ -n "$(BASE)" ] && git rev-parse -q --verify "$(BASE)^{commit}" >/dev/null; then \
	  changed=$$(git diff --name-only "$(BASE)" HEAD); pkgs=""; \
	  for p in $(SAFETY); do d=$${p#./}; if echo "$$changed" | grep -q "^$$d/"; then pkgs="$$pkgs $$p"; fi; done; \
	else echo "safety-race: no usable BASE ($(BASE)), running all packages"; pkgs="$(SAFETY)"; fi; \
	if [ -z "$$pkgs" ]; then echo "safety-race: no safety package changed"; exit 0; fi; \
	echo "== race$$pkgs"; $(NICE) tools/race.sh $$pkgs

safety: safety-race mutation-changed

# Quarantined Go tests (internal/testutil/quarantine) of every package that has one, with up to
# three attempts. Nightly only; never part of check or ci.
test-quarantine:
	@pkgs=$$(grep -rl --include='*_test.go' 'quarantine\.Test(t' cmd internal | xargs -r -n1 dirname | sort -u | sed 's|^|./|'); \
	if [ -z "$$pkgs" ]; then echo "test-quarantine: no quarantined Go test"; exit 0; fi; \
	for i in 1 2 3; do \
	  TASMOFLEET_QUARANTINE=1 $(NICE) env CGO_ENABLED=1 $(GO) test -race -count=1 $$pkgs && exit 0; \
	  echo "test-quarantine: attempt $$i failed"; \
	done; exit 1

# Integration tests (build tag "integration"): simulator fleet, embedded MQTT
# broker, mock OIDC provider, real SQLite files. Still loopback only.
integration:
	$(NICE) $(GO) test $(TESTFLAGS) -tags=integration ./...

# Sharding (CI): with MUTATION_SHARD=i/n and MUTATION_COUNTS=<dir>, mutation and mutation-changed
# mutate only shard i of every package's files and write the counts to <dir>/<package name>;
# "make mutation-sum MUTATION_COUNTS=<dir> MUTATION_SHARDS=n" then gates each package on the sum
# of its n shards (tools/mutation-gate.sh --sum), exactly as an unsharded run would.
MUTATION_SHARD  ?=
MUTATION_SHARDS ?=
MUTATION_COUNTS ?=
mutation_gate = TOOL="$(NICE) $(TOOL)" $(if $(MUTATION_SHARD),MUTATION_SHARD=$(MUTATION_SHARD) MUTATION_COUNTS_OUT="$(MUTATION_COUNTS)/$$(basename $$p)") tools/mutation-gate.sh

mutation:
	@$(if $(MUTATION_SHARD),mkdir -p "$(MUTATION_COUNTS)";) for e in $(MUTATION); do \
	  p=$${e%%:*}; $(mutation_gate) "$$p" "$${e##*:}" || exit 1; \
	done

# Mutation gate only for the MUTATION packages with changes since BASE (a git
# ref). CI runs it for PRs that change a SAFETY package; the FULL tier runs "mutation".
# Without a usable BASE every package runs.
mutation-changed:
	@if [ -z "$(BASE)" ] || ! git rev-parse -q --verify "$(BASE)^{commit}" >/dev/null; then \
	  echo "mutation-changed: no usable BASE ($(BASE)), running all packages"; $(MAKE) --no-print-directory mutation; exit $$?; \
	fi; \
	$(if $(MUTATION_SHARD),mkdir -p "$(MUTATION_COUNTS)";) \
	changed=$$(git diff --name-only "$(BASE)" HEAD); ran=0; \
	for e in $(MUTATION); do \
	  p=$${e%%:*}; d=$${p#./}; \
	  if echo "$$changed" | grep -q "^$$d/"; then ran=1; $(mutation_gate) "$$p" "$${e##*:}" || exit 1; \
	  else echo "== mutation $$p: unchanged since $(BASE), skipped"; fi; \
	done; [ $$ran = 1 ] || echo "mutation-changed: no mutation package changed"

# Gate of a sharded run: MUTATION_COUNTS holds one directory per shard (CI artifacts). A package
# without counts was not run (unchanged); with MUTATION_ALL=1 (the FULL tier) every package must
# have them. A package with counts from fewer than MUTATION_SHARDS shards fails.
mutation-sum:
	@test -n "$(MUTATION_COUNTS)" && test -n "$(MUTATION_SHARDS)" || { echo "usage: make mutation-sum MUTATION_COUNTS=<dir> MUTATION_SHARDS=n" >&2; exit 2; }; \
	for e in $(MUTATION); do \
	  p=$${e%%:*}; files=$$(ls "$(MUTATION_COUNTS)"/*/"$$(basename $$p)" 2>/dev/null || true); n=$$(echo "$$files" | grep -c . || true); \
	  if [ "$$n" = 0 ] && [ "$(MUTATION_ALL)" != 1 ]; then echo "== mutation $$p: not run (unchanged)"; continue; fi; \
	  [ "$$n" = "$(MUTATION_SHARDS)" ] || { echo "mutation-sum: $$p has counts from $$n of $(MUTATION_SHARDS) shards" >&2; exit 1; }; \
	  tools/mutation-gate.sh --sum "$$p" "$${e##*:}" $$files || exit 1; \
	done

e2e-deps:
	cd e2e && npm ci

# Playwright against the real binary + tasmofleet-devenv (simulators, broker, mock IdP).
# e2e is the whole behaviour suite (FULL tier), e2e-core the tests tagged @core (FAST tier, every
# PR), e2e-quarantine the tests tagged @quarantine with retries (nightly only). Tag a test with
# test("title", { tag: "@core" }, ...); see CLAUDE.md "Running the gates" for what belongs in core.
# Two Playwright runs, always both, and the target fails if either failed: the "chromium" project
# (E2E_ARGS, e.g. --shard) and the "mqtt" project (one worker, never sharded; E2E_MQTT_ARGS). A CI shard
# that must not repeat it sets E2E_MQTT=0.
E2E_MQTT ?= 1
define playwright-both
	@rc=0; \
	(cd e2e && npx playwright test --project=chromium $(1) $(E2E_ARGS)) || rc=1; \
	if [ "$(E2E_MQTT)" = 1 ]; then (cd e2e && npx playwright test --project=mqtt $(1) $(E2E_MQTT_ARGS)) || rc=1; fi; \
	exit $$rc
endef

e2e: build
	$(call playwright-both,--grep-invert @quarantine)

e2e-core: build
	$(call playwright-both,--grep @core --grep-invert @quarantine)

e2e-quarantine: build
	cd e2e && npx playwright test --project=chromium --grep @quarantine --retries=2 --pass-with-no-tests $(E2E_ARGS)

# Pixel comparison of our pages against the Claude Design prototype (design/v2/README.md). Not part of CI: the references (PROTO_REF_DIR, ~150 MB) live outside the repository and
# design/v2/ref-manifest.sha256 pins them. Runs the tool's self-test first, then every reference (or the
# entries whose owner or id contains a word of SCREENS, e.g. SCREENS=P2,device-console), with at most two
# pages at a time on the desktop. The report is e2e/test-results/visual/report.html. Expected to fail for
# every screen whose work package has not landed.
PROTO_REF_DIR ?=
VISUAL_WORKERS ?= 2
visual: build
	@test -n "$(PROTO_REF_DIR)" || { echo "set PROTO_REF_DIR=<dir with the Claude Design reference screenshots> (see design/v2/README.md)" >&2; exit 2; }
	rm -rf e2e/test-results/visual e2e/test-results/visual-shell e2e/test-results/visual-selftest
	cd e2e && PROTO_REF_DIR="$(PROTO_REF_DIR)" nice -n 10 npx playwright test --project=visual --workers=$(VISUAL_WORKERS) visual-selftest \
	  || { echo "visual: the tool's self-test failed, page results would mean nothing"; exit 1; }
	cd e2e && rc=0; VISUAL_STACKS=1 PROTO_REF_DIR="$(PROTO_REF_DIR)" SCREENS="$(SCREENS)" nice -n 10 npx playwright test --project=visual --workers=$(VISUAL_WORKERS) visual-v2 || rc=$$?; \
	node visual-report.mjs; exit $$rc

# The style guide in the repository (plan D18): one screenshot per /styleguide section, desktop and
# phone, light and dark, into docs/design/styleguide/ (embedded by docs/spec/design-system.md). Run it
# after changing a component and commit the images; a second run writes the same files.
# styleguide_doc_test.go fails when a section has no doc entry or no screenshot. Not part of CI.
styleguide-docs: build
	cd e2e && nice -n 10 npx playwright test --project=docs --workers=$(VISUAL_WORKERS) styleguide-docs

# Parity of a package that must not change how pages look (plan 4.2, WP F1): renders every page of the V0
# entry table (no clicks) at 1280/375 px, light and dark, once with binaries built from BASE (a commit) and
# once with the current ones, and compares the sets with tolerance 0. Fails on any differing pixel of a page that is stable in the base.
# The base is rendered twice: values the app invents per run (simulator ports, rows ordered by time) make
# some pages differ from themselves, and only pages that are stable in the base are decided by pixels.
# Both sides are built with VERSION=parity, because the About page prints the version.
# Output: e2e/parity/{base,base2,head,diff} (not under test-results: Playwright empties that directory on every run). Two pages at a time, like make visual;
# SCREENS narrows the pages as for make visual (e.g. SCREENS=settings-advanced,devices).
PARITY_TMP := $(shell mktemp -d -u /tmp/tasmofleet-parity.XXXXXX)
parity_out := $(CURDIR)/e2e/parity
visual-parity:
	@test -n "$(PROTO_REF_DIR)" || { echo "set PROTO_REF_DIR=<dir with the Claude Design reference screenshots> (see design/v2/README.md)" >&2; exit 2; }
	@test -n "$(BASE)" || { echo "usage: make visual-parity BASE=<commit>"; exit 2; }
	rm -rf $(parity_out); mkdir -p $(parity_out)
	git worktree add --detach "$(PARITY_TMP)" "$(BASE)"
	$(MAKE) build VERSION=parity
	cd "$(PARITY_TMP)" && $(MAKE) build VERSION=parity || { git worktree remove --force "$(PARITY_TMP)"; exit 1; }
	cd e2e && rc=0; \
	for run in base base2 head; do \
	  if [ $$run = head ]; then bins="$(CURDIR)/bin"; else bins="$(PARITY_TMP)/bin"; fi; \
	  rm -rf test-results/visual; \
	  E2E_BIN_DIR="$$bins" VISUAL_STACKS=1 PARITY_OUT="$(parity_out)/$$run" PROTO_REF_DIR="$(PROTO_REF_DIR)" SCREENS="$(SCREENS)" \
	    nice -n 10 npx playwright test --project=visual --workers=$(VISUAL_WORKERS) visual-parity || rc=$$?; \
	done; \
	git worktree remove --force "$(PARITY_TMP)"; \
	[ $$rc = 0 ] || exit $$rc; \
	node visual-parity.mjs "$(parity_out)/base" "$(parity_out)/base2" "$(parity_out)/head" "$(parity_out)/diff"

# The static demo for GitHub Pages (e2e/tests/demo-site.spec.ts): the design fixture fleet (60 simulated
# devices) rendered by the real binaries, every page an admin reaches by plain links saved as
# static HTML into dist/demo (not committed; the pages workflow builds and publishes it).
demo: build
	cd e2e && nice -n 10 npx playwright test --project=demo --workers=1 -g "static demo site"

# The README media in docs/media: screenshots of the same fleet and the update animation
# (demo.webp, recorded with Playwright, converted with ffmpeg). Run after a visible UI change and commit the files.
demo-media: build
	cd e2e && DEMO_MEDIA=1 nice -n 10 npx playwright test --project=demo --workers=2
	nice -n 10 ffmpeg -loglevel error -y -i e2e/test-results/demo.webm \
	  -vf "fps=8,scale=960:-1:flags=lanczos" -c:v libwebp_anim -lossless 0 -q:v 60 -compression_level 6 -loop 0 docs/media/demo.webp
	@ls -l docs/media

vuln:
	$(NICE) $(TOOL) govulncheck ./...

# Tasmota command catalogues (internal/tasmotacmd, docs/research/tasmota-command-catalogue.md): one
# catalogue per pinned Tasmota version, so the policy editor can judge a command against the
# firmware versions in the fleet (#269). The Tasmota source of a version is fetched by catalogue-src
# only (never by a test or a normal build), checked against its sha256 and unpacked next to the
# user's caches, not into the repository. To pin another version: add "version=sha256" to
# TASMOTA_PINS and run `make catalogue`; to drop one, remove it and delete its catalogue-*.json
# (catalogue-check fails on a file without a pin). The newest pin is the one used for a version
# newer than every pin.
TASMOTA_PINS ?= \
  12.5.0=0c89a141b0db569603f224248c45229c45ca8c5b8ef0d598e7343c78e874524d \
  13.4.0=869dc19e0e2c2f04e8eaa43f62e6486832b3f24e57c226fa4825502bee6a6885 \
  14.4.1=beb873eea61944fb9aaec8e0b943e21be7a6527723b2c2873b993c90e449d0ac \
  15.6.0=91365e7308ec45aee6e7bef8debfb78fbadf5315a1c5d7529ffd87de198b8d4f \
  16.0.0=d2812ba1b7a34c17dd32b4aab46de30a4f777a83ae5cce2c0d60176d34bf0f2c
TASMOTA_CACHE ?= $(or $(XDG_CACHE_HOME),$(HOME)/.cache)/tasmofleet

catalogue-src:
	@set -e; for pin in $(TASMOTA_PINS); do v=$${pin%%=*}; sha=$${pin#*=}; src="$(TASMOTA_CACHE)/tasmota-$$v"; \
	if [ ! -f "$$src/.sha256" ] || [ "$$(cat "$$src/.sha256")" != "$$sha" ]; then \
	  rm -rf "$$src"; mkdir -p "$$src"; \
	  curl -fsSL --retry 3 -o "$$src.tar.gz" "https://github.com/arendst/Tasmota/archive/refs/tags/v$$v.tar.gz"; \
	  echo "$$sha  $$src.tar.gz" | sha256sum -c - || { echo "catalogue-src: sha256 mismatch: the upstream archive of v$$v changed (or the download is damaged); do not update its pin without reading why" >&2; rm -f "$$src.tar.gz"; exit 1; }; \
	  tar -xzf "$$src.tar.gz" -C "$$src" --strip-components=1 "Tasmota-$$v/tasmota"; \
	  rm -f "$$src.tar.gz"; echo "$$sha" > "$$src/.sha256"; \
	fi; done

# Regenerates the checked-in catalogues from the pinned sources (deterministic).
catalogue: catalogue-src
	@set -e; for pin in $(TASMOTA_PINS); do v=$${pin%%=*}; sha=$${pin#*=}; \
	$(NICE) $(GO) run ./cmd/tasmofleet-catalogue -src "$(TASMOTA_CACHE)/tasmota-$$v/tasmota" -version $$v -sha256 $$sha -out internal/tasmotacmd/catalogue-$$v.json; done

# Fails when a checked-in catalogue differs from a regeneration, or a catalogue file has no pin (FULL tier).
catalogue-check: catalogue-src
	@set -e; tmp=$$(mktemp); trap 'rm -f "$$tmp"' EXIT; \
	for pin in $(TASMOTA_PINS); do v=$${pin%%=*}; sha=$${pin#*=}; \
	$(NICE) $(GO) run ./cmd/tasmofleet-catalogue -src "$(TASMOTA_CACHE)/tasmota-$$v/tasmota" -version $$v -sha256 $$sha -out "$$tmp" >/dev/null && \
	diff -u internal/tasmotacmd/catalogue-$$v.json "$$tmp" || { echo "catalogue-check: internal/tasmotacmd/catalogue-$$v.json is stale; run 'make catalogue' and commit it" >&2; exit 1; }; done; \
	for f in internal/tasmotacmd/catalogue-*.json; do v=$${f#internal/tasmotacmd/catalogue-}; v=$${v%.json}; \
	case " $(TASMOTA_PINS) " in *" $$v="*) ;; *) echo "catalogue-check: $$f has no pin in TASMOTA_PINS" >&2; exit 1;; esac; done

# The anchors of the Tasmota docs' Commands page (the help links of the policy editor, #269): the page at
# a pinned commit of tasmota/docs, its sha256 checked, reduced to the ids and heading slugs a link can
# point to. A test fails when a link the editor builds is not in this list; catalogue-check's sibling
# docs-anchors-check fails when the list differs from the pinned page. Only the page's anchors are kept,
# none of its text.
TASMOTA_DOCS_COMMIT ?= 1c05b9917d284470332fedd8cade3a3f756b0e8b
TASMOTA_DOCS_SHA256 ?= e2f1a30c8723d60f0d09de349ca74a6a45c1e17844706dde2c388912d8bc1deb
DOCS_ANCHORS       := internal/tasmotacmd/docs-anchors.txt

define docs_anchors_gen
set -e; page=$$(mktemp); trap 'rm -f "$$page"' EXIT; \
curl -fsSL --retry 3 -o "$$page" "https://raw.githubusercontent.com/tasmota/docs/$(TASMOTA_DOCS_COMMIT)/docs/Commands.md"; \
echo "$(TASMOTA_DOCS_SHA256)  $$page" | sha256sum -c - >/dev/null || { echo "docs-anchors: sha256 mismatch for Commands.md at $(TASMOTA_DOCS_COMMIT)" >&2; exit 1; }; \
echo "# Anchors of docs/Commands.md of tasmota/docs at $(TASMOTA_DOCS_COMMIT) (sha256 $(TASMOTA_DOCS_SHA256)). Generated by make docs-anchors."; \
{ grep -o 'id="[^"]*"' "$$page" | sed 's/^id="//; s/"$$//'; \
  sed -n 's/^##\{1,3\} //p' "$$page" | tr 'A-Z' 'a-z' | sed 's/[^a-z0-9 _-]//g; s/ /-/g'; } | LC_ALL=C sort -u
endef

docs-anchors:
	@($(docs_anchors_gen)) > $(DOCS_ANCHORS)

docs-anchors-check:
	@tmp=$$(mktemp); trap 'rm -f "$$tmp"' EXIT; ($(docs_anchors_gen)) > "$$tmp" && diff -u $(DOCS_ANCHORS) "$$tmp" || { echo "docs-anchors-check: $(DOCS_ANCHORS) differs from the pinned page; run 'make docs-anchors'" >&2; exit 1; }

build:
	CGO_ENABLED=0 $(NICE) $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/tasmofleet ./cmd/tasmofleet
	CGO_ENABLED=0 $(NICE) $(GO) build -trimpath -o $(BIN)/tasmofleet-devenv ./cmd/tasmofleet-devenv

image:
	docker build --build-arg VERSION=$(VERSION) -t tasmofleet:$(VERSION) .

# Release guard: fails when CHANGELOG.md has no "## X.Y.Z" section for TAG (vX.Y.Z).
# The heading is "## X.Y.Z (date)" or, for a named release, "## X.Y.Z "Name" (date)".
# The tag build in the CI image workflow calls it.
changelog-check:
	@test -n "$(TAG)" || { echo "usage: make changelog-check TAG=vX.Y.Z" >&2; exit 2; }
	@v='$(TAG)'; v=$${v#v}; \
	grep -Eq "^## $$(printf '%s' "$$v" | sed 's/[.]/[.]/g')( \"[^\"]+\")?( \(.*\))?$$" CHANGELOG.md || { \
		echo "CHANGELOG.md has no section '## $$v' for tag $(TAG): move \"Unreleased\" into it before tagging" >&2; exit 1; }

# Prints the GitHub release title for TAG: TasmoFleet X.Y.Z "Name" when the CHANGELOG
# heading carries a name, TasmoFleet X.Y.Z otherwise.
changelog-title: changelog-check
	@v='$(TAG)'; v=$${v#v}; \
	name=$$(grep -E "^## $$(printf '%s' "$$v" | sed 's/[.]/[.]/g') \"[^\"]+\"" CHANGELOG.md | head -n1 | sed -E 's/^[^"]*"([^"]+)".*$$/\1/'); \
	if [ -n "$$name" ]; then echo "TasmoFleet $$v \"$$name\""; else echo "TasmoFleet $$v"; fi

# Release guard: a patch release (vX.Y.Z, Z>0) never adds or changes a database migration.
# Fails when internal/store/migrations/ differs between vX.Y.0 and REV (default HEAD).
migration-guard:
	@test -n "$(TAG)" || { echo "usage: make migration-guard TAG=vX.Y.Z [REV=<commit>]" >&2; exit 2; }
	@v='$(TAG)'; v=$${v#v}; v=$${v%%-*}; z=$${v##*.}; base=v$${v%.*}.0; \
	if [ "$$z" -eq 0 ]; then exit 0; fi; \
	git rev-parse -q --verify "refs/tags/$$base^{commit}" >/dev/null || { \
		echo "patch release $(TAG) needs tag $$base to compare migrations against, but it does not exist" >&2; exit 1; }; \
	changed=$$(git diff --name-only "$$base..$(or $(REV),HEAD)" -- internal/store/migrations/); \
	if [ -n "$$changed" ]; then \
		echo "patch release $(TAG) changes database migrations since $$base (a schema change means a new minor version):" >&2; \
		echo "$$changed" >&2; exit 1; fi

# Prints the body of the CHANGELOG.md section for TAG (the GitHub release notes).
changelog-notes: changelog-check
	@v='$(TAG)'; v=$${v#v}; \
	awk -v v="$$v" '$$0 == "## " v || index($$0, "## " v " ") == 1 { on = 1; next } \
		on && /^## / { exit } on' CHANGELOG.md

tools:
	$(TOOL) -n staticcheck >/dev/null

clean:
	rm -rf $(BIN) e2e/test-results e2e/playwright-report
