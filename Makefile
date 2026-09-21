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

.PHONY: build deps test vet fmt tidy tidy-check lint vuln check release-check \
	release release-root release-submodules clean

build:
	$(GO) build ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GO) build ./...) || exit 1; done

# The root module is the session vocabulary and must build from
# openresponses and the standard library alone. Anything that needs
# another dependency is a nested module.
deps:
	@deps=$$($(GO) list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./... | grep -v '^github.com/ChristopherDavenport/agentsession' | grep -v '^github.com/ChristopherDavenport/openresponses' || true); \
	  test -z "$$deps" || { echo "root module depends on: $$deps"; exit 1; }

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
check: fmt tidy-check vet deps lint vuln test

# Builds and tests each nested module outside the workspace, against the
# root and sibling versions its own go.mod requires, which is what a
# consumer gets: a replace is a property of the main module and consumers
# ignore it, so a nested module that carried one would build green here
# and break on the way in. Run it before tagging a module; it fails while
# a module depends on root changes that are not tagged yet, and that
# failure is the signal to tag the root first. Not part of check: between
# a root API addition and the next root tag it fails by design.
release-check:
	@for m in $(SUBMODULES); do (cd $$m && GOWORK=off $(GO) vet ./... && GOWORK=off $(GO) test ./...) || exit 1; done

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

# Phase two, once release-root has pushed the root tag: point each nested
# module at VERSION and release it, one at a time in SUBMODULES order.
# One commit and one tag per module, because go mod tidy and
# release-check both resolve a sibling requirement from the proxy, so a
# module has to be tagged and pushed before the module that requires it
# is bumped. Each module is built the way a consumer builds it before its
# tag is written, which is the whole point of the phasing.
release-submodules:
	@test -n "$(VERSION)" || { echo "usage: make release-submodules VERSION=vX.Y.Z"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "working tree is not clean"; exit 1; }
	@git rev-parse -q --verify refs/tags/$(VERSION) >/dev/null || { echo "$(VERSION) is not tagged; run make release-root first"; exit 1; }
	@awk -v v="$(VERSION)" '/^## /{p=($$2==v)} p' CHANGELOG.md | sed '1s/.*/$(VERSION)/' > $(NOTES)
	@for m in $(SUBMODULES); do ( \
	  cd $$m && $(GO) mod edit -require=$(MODULE)@$(VERSION) && \
	  for s in $(SUBMODULES); do \
	    if grep -q "^[[:space:]]*$(MODULE)/$$s " go.mod; then $(GO) mod edit -require=$(MODULE)/$$s@$(VERSION) || exit 1; fi; \
	  done && $(GO) mod tidy && \
	  GOWORK=off $(GO) vet ./... && GOWORK=off $(GO) test ./... ) || exit 1; \
	  git add -A && git commit -q -m "Release $$m/$(VERSION)" $(if $(TRAILER),-m "$(TRAILER)") || exit 1; \
	  git tag -a $$m/$(VERSION) -F $(NOTES) || exit 1; \
	  git push origin HEAD && git push origin $$m/$(VERSION) || exit 1; \
	done
	@rm -f $(NOTES)
	$(MAKE) fmt tidy-check

clean:
	rm -rf .cache
