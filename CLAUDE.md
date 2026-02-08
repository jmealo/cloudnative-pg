# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository Overview

CloudNativePG (CNPG) is a Kubernetes operator for managing PostgreSQL databases, written in Go. It's a CNCF Sandbox project that provides native PostgreSQL cluster management in Kubernetes environments.

## Key Commands

### Build Commands
- `make build` - Build both manager and kubectl plugin binaries
- `make build-manager` - Build the operator binary
- `make build-plugin` - Build the kubectl-cnpg plugin
- `make docker-build` - Build Docker images using goreleaser

### Testing Commands
- `make test` - Run unit tests with coverage
- `make test-race` - Run tests with race detection
- `make e2e-test-kind` - Run E2E tests locally using kind (recommended for development)
- `make e2e-test-local` - Run E2E tests on current kubernetes context

To run a single test:
- Unit test: `go test -v ./path/to/package -run TestName`
- E2E test: `make e2e-test-kind E2E_TEST_TAGS="TestSuiteName"`

### Development Commands
- `make generate` - Generate code (deepcopy, CRDs, manifests) - run after API changes
- `make manifests` - Generate CRDs and RBAC manifests
- `make fmt` - Format Go code
- `make lint` - Run golangci-lint
- `make lint-fix` - Auto-fix linting issues
- `make checks` - Run all quality checks before committing

### Before Committing
Always run: `make generate manifests fmt lint test`

## Architecture and Code Structure

### Core Components
- **cmd/manager/** - The operator's main entry point that runs the controller manager
- **cmd/kubectl-cnpg/** - The kubectl plugin for interacting with CNPG resources
- **api/v1/** - Contains all Custom Resource Definitions (CRDs):
  - `cluster_types.go` - Main PostgreSQL cluster resource
  - `backup_types.go` - Backup and ScheduledBackup resources
  - `pooler_types.go` - PgBouncer pooler configuration
  - `database_types.go` - Declarative database management
  - `publication_types.go` & `subscription_types.go` - Logical replication

### Controller Pattern
- **internal/controller/** - Contains reconciliation logic for each CRD
- Controllers follow the standard Kubernetes controller pattern with reconciliation loops
- Each controller is responsible for moving resources from current state to desired state
- Use controller-runtime's client for all Kubernetes API interactions

### Webhook System
- **internal/webhook/** - Validation and mutation webhooks for each resource type
- Webhooks provide admission control before resources are stored
- Follow the pattern of implementing `ValidateCreate`, `ValidateUpdate`, and `ValidateDelete`

### Testing Approach
- Unit tests are co-located with source files (*_test.go)
- E2E tests are in **tests/e2e/** and use Ginkgo/Gomega
- Mock Kubernetes clients using controller-runtime's fake client
- Test files should follow the pattern of testing public interfaces

### Key Conventions
- All commits must be signed-off (DCO) and follow conventional commits format
- Feature branches should be named `dev/ISSUE_ID`
- Code must pass all linters configured in `.golangci.yml`
- New features require both unit tests and E2E tests
- API changes require running `make generate manifests`

### Important Patterns
- The operator uses a declarative approach - users define desired state in CRDs
- Reconciliation is idempotent and should handle partial failures gracefully
- Status conditions follow Kubernetes conventions (True/False/Unknown)
- Use structured logging with the controller-runtime logger
- Resource ownership is managed through Kubernetes owner references

### Plugin Development
- The kubectl plugin uses Cobra for CLI structure
- Plugin commands are in **cmd/kubectl-cnpg/cmd/**
- Follow existing command patterns for consistency
- Use the same client libraries as the operator for API interactions