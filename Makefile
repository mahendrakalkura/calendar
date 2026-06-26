.DEFAULT_GOAL := help
.PHONY: build help lint run

GOCACHE ?= $(CURDIR)/.gocache
GOLANGCI_LINT_CACHE ?= $(CURDIR)/.golangci-lint-cache

TZARG := $(firstword $(filter-out run force,$(MAKECMDGOALS)))
FORCE := $(if $(filter force,$(MAKECMDGOALS)),--force)

build: ## Build the binary
	@go mod tidy
	@go build -o ./main .

help: ## Display this help message
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-8s\033[0m %s\n", $$1, $$2}'

lint: ## Run golangci-lint
	@mkdir -p $(GOCACHE)
	@mkdir -p $(GOLANGCI_LINT_CACHE)
	@GOCACHE=$(GOCACHE) GOLANGCI_LINT_CACHE=$(GOLANGCI_LINT_CACHE) golangci-lint run ./...

run: build ## Build and run (e.g. make run IST, make run PST force)
	@./main $(if $(TZARG),--tz=$(TZARG)) $(FORCE)

%:
	@:
