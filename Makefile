HELM ?= helm
CHART_DIR ?= charts/uni-replicator
RELEASE_NAME ?= uni-replicator
NAMESPACE ?= uni-replicator
VALUES ?=
HELM_ARGS ?=

HELM_VALUES = $(if $(strip $(VALUES)),--values "$(VALUES)",)

.PHONY: check fmt-check test vet build helm-lint helm-template

check: fmt-check test vet build helm-lint

fmt-check:
	@unformatted="$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"; \
	test -z "$$unformatted" || { echo "run gofmt on:"; echo "$$unformatted"; exit 1; }

test:
	@go test ./...

vet:
	@go vet ./...

build:
	@go build ./...

helm-lint:
	@$(HELM) lint "$(CHART_DIR)" $(HELM_VALUES) $(HELM_ARGS)

helm-template:
	@$(HELM) template "$(RELEASE_NAME)" "$(CHART_DIR)" \
		--namespace "$(NAMESPACE)" \
		$(HELM_VALUES) $(HELM_ARGS)
