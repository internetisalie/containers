GOPATH = $(shell go env GOPATH)
GOBIN = $(GOPATH)/bin
BUILDER = $(GOBIN)/glimmer

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

release:
	$(BUILDER) $@ $(ARGS)

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
