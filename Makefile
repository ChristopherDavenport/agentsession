GO ?= go
STATICCHECK ?= $(GO) run honnef.co/go/tools/cmd/staticcheck@latest
GOVULNCHECK ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@latest
# Nested modules with their own go.mod, so their dependencies stay out of
# the root module. Each requires a released root version and carries no
# replace; go.work builds them against the tree, and release-check builds
# them the way a consumer does. Every module shares one version: see
# release-root and release-submodules. ./... from the root covers only the
# root module, so every target loops over them. Listed in dependency
# order, because release-submodules tags them in this order.
SUBMODULES = sqlite otel

# Which modules release-check and release-submodules act on. SUBMODULES
# stays the full set: release-submodules needs it to find the siblings a
# module requires, so narrowing that instead would quietly skip a sibling
# bump. Override this one to release or check a single module, which is
# what the release workflow does for the tag it fires on.
RELEASE_SUBMODULES ?= $(SUBMODULES)

.PHONY: build deps no-replace test vet fmt tidy tidy-check lint vuln check \
	release-check release release-root release-submodules clean

build:
	$(GO) build ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GO) build ./...) || exit 1; done

# The root module is the session vocabulary and must build from
# openresponses and the standard library alone. Anything that needs
# another dependency is a nested module.
deps:
	@deps=$$($(GO) list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./... | grep -v '^github.com/ChristopherDavenport/agentsession' | grep -v '^github.com/ChristopherDavenport/openresponses' || true); \
	  test -z "$$deps" || { echo "root module depends on: $$deps"; exit 1; }

# No module in the repository may replace a first-party one. A replace is
# a property of the main module and consumers ignore it, so a nested
# module carrying one builds green everywhere here — including under
# release-check, which is the whole point of release-check — while
# shipping a go.mod that names a version it was never built against.
# go.work is how the tree is built against the tree. This runs in check
# rather than only at release because re-adding a replace is exactly how
# the hole opens, and one make tidy afterwards settles every other gate.
# There is deliberately no opt-out: a module that genuinely needs one is
# a change to this target, reviewable in the diff.
no-replace:
	@for m in . $(SUBMODULES); do \
	  if grep -v '^[[:space:]]*//' $$m/go.mod | grep -q 'github.com/ChristopherDavenport/.*=>'; then \
	    echo "$$m/go.mod replaces a first-party module; nested modules require published versions and go.work builds them against the tree (see CONTRIBUTING.md)"; \
	    exit 1; \
	  fi; \
	done

test:
	$(GO) test -race ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GO) test -race ./...) || exit 1; done

vet:
	$(GO) vet ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GO) vet ./...) || exit 1; done

tidy:
	$(GO) mod tidy
	@for m in $(SUBMODULES); do (cd $$m && $(GO) mod tidy) || exit 1; done

# Fails when go mod tidy would change any go.mod or go.sum, without
# writing, so a stray dependency shows up in make check and not only in
# CI's diff.
tidy-check:
	$(GO) mod tidy -diff
	@for m in $(SUBMODULES); do (cd $$m && $(GO) mod tidy -diff) || exit 1; done

fmt:
	gofmt -l . && test -z "$$(gofmt -l .)"

lint:
	$(STATICCHECK) ./...
	@for m in $(SUBMODULES); do (cd $$m && $(STATICCHECK) ./...) || exit 1; done

vuln:
	$(GOVULNCHECK) ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GOVULNCHECK) ./...) || exit 1; done

# Everything CI runs.
check: fmt tidy-check vet deps no-replace lint vuln test

# Builds and tests each module in RELEASE_SUBMODULES outside the
# workspace, against the root and sibling versions its own go.mod
# requires, which is what a consumer gets. Run it before tagging a
# module; it fails while a module depends on root changes that are not
# tagged yet, and that failure is the signal to tag the root first. Not
# part of check: between a root API addition and the next root tag it
# fails by design. no-replace is what keeps this honest — a first-party
# replace would make it pass on a module no consumer can build.
release-check:
	@test -n "$(RELEASE_SUBMODULES)" || { echo "RELEASE_SUBMODULES is empty; nothing would be checked"; exit 1; }
	@for m in $(RELEASE_SUBMODULES); do (cd $$m && GOWORK=off $(GO) vet ./... && GOWORK=off $(GO) test ./...) || exit 1; done

# go list -m reports every module in the workspace, so the root has to be
# asked for outside it.
MODULE := $(shell GOWORK=off $(GO) list -m)
NOTES := $(shell mktemp)

# Cut a release. Every module in the repository shares one version, but
# not one commit: a nested module's requirement cannot name a tag that
# does not exist yet, and release-check resolves that requirement from
# the proxy rather than from the workspace. So the root is released and
# tagged first, and each nested module follows once everything it
# requires is published.
release:
	@echo "release is phased, because a require cannot name an unpublished tag:"; \
	 echo "    make release-root VERSION=vX.Y.Z"; \
	 echo "    make release-submodules VERSION=vX.Y.Z"; \
	 exit 1

# Phase one: date the changelog, check everything, commit, tag the root
# and push. The nested modules still require the previous root release
# across this commit, which is correct until this tag exists.
# TRAILER, when set, is appended to the commit message.
release-root:
	@test -n "$(VERSION)" || { echo "usage: make release-root VERSION=vX.Y.Z"; exit 1; }
	@grep -q '^## Unreleased$$' CHANGELOG.md || { echo "CHANGELOG.md has no Unreleased section"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "working tree is not clean"; exit 1; }
	sed -i 's/^## Unreleased$$/## $(VERSION) - '"$$(date +%F)"'/' CHANGELOG.md
	$(MAKE) tidy
	$(MAKE) check
	git add -A && git commit -q -m "Release $(VERSION)" $(if $(TRAILER),-m "$(TRAILER)")
	@awk -v v="$(VERSION)" '/^## /{p=($$2==v)} p' CHANGELOG.md | sed '1s/.*/$(VERSION)/' > $(NOTES)
	git tag -a $(VERSION) -F $(NOTES)
	@rm -f $(NOTES)
	git push origin HEAD
	git push origin $(VERSION)

# Phase two, once release-root has pushed the root tag: point each module
# in RELEASE_SUBMODULES at VERSION and release it, one at a time in that
# order. One commit and one tag per module, because go mod tidy and
# release-check both resolve a sibling requirement from the proxy, so a
# module has to be tagged and pushed before the module that requires it
# is bumped. Each module is built the way a consumer builds it before its
# tag is written, which is the whole point of the phasing.
#
# The sibling loop reads SUBMODULES, not RELEASE_SUBMODULES: narrowing
# the release list must not narrow the set of siblings to bump, or a
# resumed run would tag a module still requiring an old sibling — which
# release-check cannot catch, because the old sibling satisfies it.
#
# go mod tidy is free to move a requirement the go mod edit above just
# set, so the resolved version is asserted before anything is tagged.
release-submodules:
	@test -n "$(VERSION)" || { echo "usage: make release-submodules VERSION=vX.Y.Z"; exit 1; }
	@test "$(origin SUBMODULES)" = file || { echo "do not override SUBMODULES here: it is the list of siblings to bump, and narrowing it would tag a module still requiring an old sibling. Narrow RELEASE_SUBMODULES instead."; exit 1; }
	@test -n "$(RELEASE_SUBMODULES)" || { echo "RELEASE_SUBMODULES is empty; nothing would be released"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "working tree is not clean; a resumed run needs git checkout -- <module> first"; exit 1; }
	@git rev-parse -q --verify refs/tags/$(VERSION) >/dev/null || { echo "$(VERSION) is not tagged; run make release-root first"; exit 1; }
	@awk -v v="$(VERSION)" '/^## /{p=($$2==v)} p' CHANGELOG.md | sed '1s/.*/$(VERSION)/' > $(NOTES)
	@set -e; for m in $(RELEASE_SUBMODULES); do \
	  ( set -e; cd $$m; \
	    $(GO) mod edit -require=$(MODULE)@$(VERSION); \
	    for s in $(SUBMODULES); do \
	      if grep -q "^[[:space:]]*$(MODULE)/$$s " go.mod; then $(GO) mod edit -require=$(MODULE)/$$s@$(VERSION); fi; \
	    done; \
	    $(GO) mod tidy; \
	    for d in $(MODULE) $(patsubst %,$(MODULE)/%,$(SUBMODULES)); do \
	      if grep -q "^[[:space:]]*$$d " go.mod; then \
	        got=$$(GOWORK=off $(GO) list -m -f '{{.Version}}' $$d); \
	        test "$$got" = "$(VERSION)" || { echo "$$m resolves $$d at $$got, not $(VERSION)"; exit 1; }; \
	      fi; \
	    done; \
	    $(GO) mod tidy -diff; \
	    GOWORK=off $(GO) vet ./...; \
	    GOWORK=off $(GO) test ./... ); \
	  git add -A; \
	  git commit -q -m "Release $$m/$(VERSION)" $(if $(TRAILER),-m "$(TRAILER)"); \
	  git tag -a $$m/$(VERSION) -F $(NOTES); \
	  git push origin HEAD; \
	  git push origin $$m/$(VERSION); \
	done
	@rm -f $(NOTES)

clean:
	rm -rf .cache
