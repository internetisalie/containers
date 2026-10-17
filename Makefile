GOPATH = $(shell go env GOPATH)
GOBIN = $(GOPATH)/bin
BUILDER = $(GOBIN)/glimmer
GORELEASER ?= goreleaser
RELEASE_MAIN_REF ?= refs/remotes/origin/main

# glimmer tasks
TASKS = test vet generate fmt vulnerability clean release publish
.PHONY: licenses  $(TASKS)

.PHONY: licenses  all clean pre-commit

all: clean vet test vulnerability

pre-commit: generate fmt

test:
	$(BUILDER) $@ $(ARGS)

vet:
	$(BUILDER) $@ $(ARGS)

generate:
	$(BUILDER) $@ $(ARGS)

fmt:
	$(BUILDER) $@ $(ARGS)

vulnerability:
	$(BUILDER) $@ $(ARGS)

clean:
	$(BUILDER) $@ $(ARGS)

# Release entry points are shared by terminals and .gitea/workflows/release.yml,
# following Loom's one-definition-per-task convention. Other Glimmer targets stay
# unchanged; this is not a wholesale build/lint/test migration.
.PHONY: release-check release-snapshot release-ref-check

release-check:
	$(GORELEASER) check
	bash .gitea/scripts/test-release-ref.sh

release-snapshot:
	$(GORELEASER) release --snapshot --clean --skip=publish

release-ref-check:
	bash .gitea/scripts/check-release-ref.sh "$(RELEASE_MAIN_REF)"

release: release-ref-check
	$(GORELEASER) release --clean $(ARGS)

publish:
	$(BUILDER) $@ $(ARGS)

licenses:
	@export GOROOT=$$(go env GOROOT); \
	GLIMMER_BIN="$(BUILDER)"; \
	first_word=$$(echo "$$GLIMMER_BIN" | cut -d' ' -f1); \
	if [ -z "$$first_word" ] || ! command -v "$$first_word" >/dev/null 2>&1; then \
		GLIMMER_BIN="glimmer"; \
	fi; \
	$$GLIMMER_BIN licenses
