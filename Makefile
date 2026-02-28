# Meta tasks
# ----------

# Go parameters (cross-compile targets: override GOARCH/GOOS as needed)
GOCMD=GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get

# collector version
OTEL_VERSION=0.146.0

# OTel Collector Builder
GOPATH := $(shell go env GOPATH)
OCB = $(GOPATH)/bin/builder
TELEMETRYGEN = $(GOPATH)/bin/telemetrygen
BUILDER_CONFIG = build/builder-config.yaml
COLLECTOR_BINARY = ./dist/otelcol-databricks

# Useful variables

# region
export AWS_REGION ?= eu-west-1

# Output helpers
# --------------

TASK_DONE = echo "✓  $@ done"
TASK_BUILD = echo "🛠️  $@ done"

export CODE_BUILD_NUMBER ?= 0
export CODE_RESOLVED_SOURCE_VERSION ?=$(shell git rev-list -1 HEAD --abbrev-commit)
export BUILD_DATE=$(shell date -u '+%Y%m%d')


collector/build: ## build vanilla otel contrib collector using ocb
	@$(OCB) --config=$(BUILDER_CONFIG)
	@$(TASK_BUILD)

collector/run: ## run locally built collector with test config (debug exporter only)
	@$(COLLECTOR_BINARY) --config=test/config.yaml

collector/run/databricks: ## run locally built collector forwarding to Databricks (requires .env)
	@set -a && . ./.env && set +a && $(COLLECTOR_BINARY) --config=test/databricks-config.yaml

collector/start: ## start collector (docker)
	@docker run -p 4317:4317 -v $(PWD)/test/config.yaml:/etc/otelcol-contrib/config.yaml otel/opentelemetry-collector-contrib:${OTEL_VERSION}
	@$(TASK_BUILD)

test/traffic/traces: ## send test traces to collector using telemetrygen
	@$(TELEMETRYGEN) traces --otlp-insecure --otlp-endpoint=localhost:4317 --duration=5s

test/traffic/metrics: ## send test metrics to collector using telemetrygen
	@$(TELEMETRYGEN) metrics --otlp-insecure --otlp-endpoint=localhost:4317 --duration=5s

test/traffic/logs: ## send test logs to collector using telemetrygen
	@$(TELEMETRYGEN) logs --otlp-insecure --otlp-endpoint=localhost:4317 --duration=5s

## Extension checks
## -----------------

EXT_DIR = extension/databricksauthextension

ext/deps: ## verify extension module dependencies
	cd $(EXT_DIR) && go mod download && go mod verify

ext/vet: ## run go vet on extension
	cd $(EXT_DIR) && go vet ./...

ext/staticcheck: ## run staticcheck on extension
	cd $(EXT_DIR) && staticcheck ./...

ext/test: ## run extension tests with race detector and coverage report
	cd $(EXT_DIR) && go test -v -race -coverprofile=coverage.out -covermode=atomic ./...

ext/test/coverage: ext/test ## display extension test coverage summary
	cd $(EXT_DIR) && go tool cover -func=coverage.out

ext/build: ## verify extension compiles
	cd $(EXT_DIR) && go build ./...

ext/security/gosec: ## run gosec security scanner on extension
	cd $(EXT_DIR) && gosec -no-fail -fmt text ./...

ext/security/govulncheck: ## run govulncheck on extension
	cd $(EXT_DIR) && govulncheck ./...

ext/ci: ext/deps ext/vet ext/staticcheck ext/test ext/build ext/security/gosec ext/security/govulncheck ## run all CI checks for the extension

clean: ## clean any generated files
	@$(GOCLEAN)
	@rm -f ./bootstrap
	@$(TASK_BUILD)

help: ## Show this help message.
	@echo 'usage: make [target] ...'
	@echo
	@echo 'targets:'
	@egrep '^(.+)\:\ ##\ (.+)' ${MAKEFILE_LIST} | column -t -c 2 -s ':#'
