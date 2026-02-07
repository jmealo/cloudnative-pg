You are finishing the PVC Auto-Resize feature for CloudNativePG.
All code is written and committed. Your job is to make it COMPILE, PASS TESTS,
BUILD, and RUN E2E on AKS.

Ref: docs/src/design/pvc-autoresize.md

## Project Context
- Repo: cloudnative-pg/cloudnative-pg (Fork by jmealo)
- Stack: Go, Controller Runtime, Kubebuilder, Ginkgo v2
- Constraints: strict linting, DCO sign-off required
- Branch: feat/pvc-autoresizing-wal-safety
- Image base: ghcr.io/jmealo/cloudnative-pg-testing
- Image tag: feat-pvc-autoresizing-<git-short-sha> (unique per build — see Phase 4)
- AKS E2E script: `hack/e2e/run-e2e-aks-autoresize.sh`

## What Already Exists (ALL COMMITTED — DO NOT REWRITE UNLESS BROKEN)

These were implemented in prior loops. You may ONLY modify them to fix compilation
or test failures. Do not add new features or restructure.

Core implementation (commit 8f9b2b1b2):
- `pkg/management/postgres/disk/probe.go` — statfs disk probe
- `pkg/management/postgres/wal/health.go` — WAL health checker
- `pkg/management/postgres/webserver/metricserver/disk.go` — Prometheus metrics
- `pkg/management/postgres/disk_status.go` — fillDiskStatus + fillWALHealthStatus
- `pkg/management/postgres/probes.go` — calls fillDiskStatus and fillWALHealthStatus
- `pkg/postgres/status.go` — DiskStatus, WALHealthStatus, WALInactiveSlotInfo types
- `api/v1/cluster_types.go` — ResizeConfiguration, ResizeTriggers, ExpansionPolicy,
  ResizeStrategy, WALSafetyPolicy, ClusterDiskStatus, InstanceDiskStatus,
  VolumeDiskStatus, WALHealthInfo, InactiveSlotInfo, AutoResizeEvent
- `pkg/reconciler/autoresize/` — reconciler.go, clamping.go, triggers.go,
  ratelimit.go, walsafety.go + all test files + suite_test.go
- `internal/webhook/v1/cluster_webhook.go` — validateAutoResize
- `internal/webhook/v1/cluster_webhook_autoresize_test.go`
- `internal/controller/cluster_controller.go` — buildDiskInfoByPod, autoresize.Reconcile

E2E tests (commits dca3c5b51 through 37fe06628):
- `tests/e2e/auto_resize_test.go` — 12 test contexts with runtime behavior
- `tests/e2e/fixtures/auto_resize/` — 9 fixture templates
- `tests/labels.go` — LabelAutoResize constant

Polish (commits b0f3fb936 through 46ab032e3):
- `docs/src/samples/monitoring/prometheusrule.yaml` — cnpg-disk.rules group (10 alerts)
- `docs/src/storage_autoresize.md` — user documentation
- `docs/src/storage.md` — cross-reference added
- `internal/cmd/plugin/disk/` — kubectl cnpg disk status command
- `cmd/kubectl-cnpg/main.go` — disk.NewCmd() registered
- `internal/controller/cluster_status.go` — updateDiskStatus function

## What Has NOT Been Done (YOUR JOB)

The previous loop wrote all the code but NEVER ran the build tooling.
None of this has been verified:
1. `make generate` — deepcopy generation after API type changes
2. `make manifests` — CRD manifest generation
3. `make fmt` — gofmt
4. `make lint` — golangci-lint
5. `make test` — unit tests
6. Docker image build + push
7. E2E test run on AKS

---

## CRITICAL: Three-Binary Architecture (Read This First)

CNPG builds THREE different binaries from the same repo. Understanding which code
runs where is essential to avoid wasting time on unnecessary platform-specific
build tags or cross-compilation fixes.

### 1. Controller Manager (`cmd/manager/main.go`)
- **Runs**: In the `cnpg-controller-manager` pod in `cnpg-system` namespace
- **Platform**: Linux only (runs in a container)
- **Contains**: Reconcilers, webhooks, autoresize logic
- **Imports**: `pkg/reconciler/autoresize/`, `internal/controller/`, `internal/webhook/`

### 2. Instance Manager (same binary as Controller, different entrypoint)
- **Runs**: Inside every PostgreSQL pod (copied by the `bootstrap-controller` init container)
- **Platform**: Linux only (runs in a container)
- **Contains**: Disk probe (statfs), WAL health checker, metrics server, webserver
- **Imports**: `pkg/management/postgres/disk/`, `pkg/management/postgres/wal/`,
  `pkg/management/postgres/webserver/`
- **How it gets there**: The init container copies `/manager` from the operator image
  into a shared volume. The PostgreSQL container runs this binary.

### 3. kubectl-cnpg Plugin (`cmd/kubectl-cnpg/main.go`)
- **Runs**: On the user's workstation (darwin, linux, windows)
- **Platform**: Cross-platform (darwin/linux/windows, amd64/arm64)
- **Contains**: CLI commands that call the Kubernetes API
- **Imports**: `internal/cmd/plugin/`, `api/v1/` (types only)
- **Does NOT import**: `pkg/management/postgres/disk/`, `pkg/management/postgres/wal/`

### What this means for you

- The `pkg/management/postgres/disk/probe.go` package uses `syscall.Statfs_t` which
  is Linux-only. This is FINE because it only runs in the instance manager (Linux).
  The kubectl plugin does NOT import this package — it reads disk status from the
  Cluster CR via the Kubernetes API. **Do NOT add platform-specific build tags
  (probe_linux.go / probe_other.go) to make the disk probe compile on darwin/windows.**
  This is unnecessary.

- **TODO**: The platform-specific split of `pkg/management/postgres/disk/probe.go`
  into `probe_linux.go` and `probe_other.go` should be reverted. The original
  single-file `probe.go` was correct because the package is never imported by the
  cross-platform kubectl plugin.

### Fixed PostgreSQL parameters

CNPG controls certain PostgreSQL parameters internally and does NOT allow users to
set them. These are listed in `pkg/postgres/configuration.go` in `FixedConfigurationParameters`.
Key ones include:
- `archive_command` — controlled by the operator; set to `/controller/manager wal-archive ...`
- `archive_mode` — always `on`
- `listen_addresses`, `port`, `log_destination`, etc.

If you try to set a fixed parameter in a Cluster spec, the webhook will reject it with:
`Can't set fixed configuration parameter`

To make WAL archiving FAIL in E2E tests (for the archive health test), configure a
`backup.barmanObjectStore` pointing to a non-existent endpoint with dummy credentials.
Without a backup config, the wal-archive command silently succeeds (no-op).

---

## The Loop (What you do on every iteration)

1. Run the next Phase step.
2. If it fails, diagnose and fix. Read error output carefully.
3. Re-run the failing step until it passes.
4. Move to the next step.
5. When you fix code, commit with DCO sign-off before moving on.

IMPORTANT: Do not rewrite working code. Only fix what is broken.

---

## Phase 1: Code Generation and Formatting

Run these in order. Each must pass before moving to the next.

- [ ] `make generate`
      This regenerates `api/v1/zz_generated.deepcopy.go`.
      If it fails: check api/v1/cluster_types.go for syntax errors in the new types.
      Common issue: missing json tags, kubebuilder markers, or pointer types.
      Fix, commit, and re-run.

- [ ] `make manifests`
      This regenerates `config/crd/bases/postgresql.cnpg.io_clusters.yaml`.
      If it fails: check for invalid kubebuilder markers on the new types.
      Fix, commit, and re-run.

- [ ] `make fmt`
      Applies gofmt + goimports.
      If it reformats files, stage and commit the formatting changes.

Phase 1 gate: `make generate && make manifests && make fmt` all exit 0.

---

## Phase 2: Linting

- [ ] `make lint`
      Runs golangci-lint.
      Common issues to expect:
      * Unused imports (especially in new files)
      * Unused variables or function parameters
      * Error return values not checked
      * Exported functions/types missing documentation comments
      * Struct field alignment (fieldalignment)
      * Cyclomatic complexity
      * Import ordering (gci)
      * Line length (lll)
      * Type assertion safety (forcetypeassert)

      Strategy:
      - Run `make lint 2>&1 | head -100` to see first batch of errors
      - Fix all errors in one file before moving to the next
      - Re-run lint after each batch of fixes
      - Commit fixes: `fix(autoresize): resolve linting issues`

Phase 2 gate: `make lint` exits 0 with no errors.

---

## Phase 3: Unit Tests

- [ ] `make test`
      Runs all unit tests via Ginkgo.
      Focus on:
      * `pkg/reconciler/autoresize/` tests (clamping, triggers, ratelimit, walsafety)
      * `internal/webhook/v1/` tests (cluster_webhook_autoresize_test.go)
      * `pkg/management/postgres/disk/` tests (probe_test.go)
      * `pkg/management/postgres/wal/` tests (health_test.go)

      If tests fail:
      - Read the failure message carefully
      - Run individual test suites to isolate:
        `go test -v ./pkg/reconciler/autoresize/...`
        `go test -v ./internal/webhook/v1/...`
      - Fix the test or the code (whichever is wrong)
      - Re-run until green
      - Commit: `fix(autoresize): resolve unit test failures`

Phase 3 gate: `make test` exits 0 with all tests passing.

---

## Phase 4: Build and Push Docker Image

### CRITICAL: How the operator image propagates to PostgreSQL pods

Understanding this architecture is essential for debugging E2E failures:

1. The **operator image** (CONTROLLER_IMG) runs as the controller-manager in cnpg-system
2. The operator sets `OPERATOR_IMAGE_NAME` env var to its own image at startup
3. When creating PostgreSQL pods, the operator adds an **init container** called
   `bootstrap-controller` that uses `OPERATOR_IMAGE_NAME` as its image
   (`pkg/specs/containers.go:37`)
4. This init container copies the **instance manager binary** (`/manager`) into a
   shared volume in the PostgreSQL pod
5. The PostgreSQL container then runs this copied instance manager binary

**This means**: Every PostgreSQL pod runs the SAME code as the operator image.
Our disk probe, WAL health checker, disk_status, and metrics code all run inside
the PostgreSQL pods via this mechanism. If the init container pulls a stale image,
the pods will run OLD code that doesn't have our changes.

### CRITICAL: Use unique image tags to avoid stale cache

The default `imagePullPolicy` in CNPG is `IfNotPresent`. If you reuse the same
tag across builds, nodes that already pulled the image will use the CACHED version.
This causes the instance manager to run old code even after rebuilding.

**The E2E script automatically generates a unique tag per build** using the git
short SHA: `feat-pvc-autoresizing-<sha>`. This ensures every rebuild produces a
fresh, unique image that Kubernetes will always pull.

Do NOT override `CONTROLLER_IMG` with a static tag like `feat-pvc-autoresizing`
unless you know the image has never been pulled on any node in the cluster.

### Two requirements for the image

1. **Multi-arch** (amd64 + arm64): The AKS cluster has mixed node pools. Do NOT
   use `make docker-build` — it forces single-arch via `--set=*.platform`.
2. **Unique tag per build**: Avoid stale image cache on nodes.

### Option A: Use the E2E script (RECOMMENDED)

The script handles multi-arch builds AND unique tagging automatically:
```
hack/e2e/run-e2e-aks-autoresize.sh
```
It auto-generates the image tag as `feat-pvc-autoresizing-$(git rev-parse --short HEAD)`.

You can override the base or tag if needed:
```
CONTROLLER_IMG_BASE=ghcr.io/jmealo/cloudnative-pg-testing \
CONTROLLER_IMG_TAG=feat-pvc-autoresizing-custom-tag \
  hack/e2e/run-e2e-aks-autoresize.sh
```

### Option B: Manual multi-arch build

If you need to build separately from testing:

```bash
# Generate a unique tag
IMG_TAG="feat-pvc-autoresizing-$(git rev-parse --short HEAD)"
IMG="ghcr.io/jmealo/cloudnative-pg-testing:${IMG_TAG}"

# 1. Install go-releaser
make go-releaser

# 2. Build Go binaries for BOTH architectures (no --single-target!)
GOOS=linux GOPATH=$(go env GOPATH) DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ") \
  COMMIT=$(git rev-parse HEAD) VERSION="" \
  bin/goreleaser build --skip=validate --clean --snapshot

# 3. Create multi-platform buildx builder (one-time)
docker buildx create --name multiarch-builder --use --platform linux/amd64,linux/arm64 || true

# 4. Build and push multi-arch image (NO --set=*.platform override)
DOCKER_BUILDKIT=1 buildVersion="" revision=$(git rev-parse HEAD) \
  docker buildx bake \
  --set distroless.tags="${IMG}" \
  --push distroless

# 5. Verify both platforms are in the manifest
docker buildx imagetools inspect "${IMG}"
```

### Common issues

- **Auth errors**: Run `echo $GHCR_TOKEN | docker login ghcr.io -u jmealo --password-stdin`
- **"no match for platform"**: You built single-arch. Rebuild using the methods above.
- **Pods running old code after rebuild**: You reused a static tag. Use a unique
  tag (git SHA) or delete the pods to force re-pull.
- **go-releaser binary not found**: Run `make go-releaser` first.
- **buildx builder doesn't support arm64**: Run `docker buildx create --name multiarch-builder --use --platform linux/amd64,linux/arm64`

### DO NOT do these things

- Do NOT use `make docker-build` — it forces single-arch
- Do NOT reuse the same image tag across builds — causes stale instance manager
- Do NOT build separate tagged images and try to merge with `docker buildx imagetools create` — fragile
- Do NOT set `ARCH=arm64` — this only builds one arch

Phase 4 gate: Multi-arch image built and pushed to GHCR with a unique tag. Verify with:
`docker buildx imagetools inspect <your-image>`
The output must show BOTH `linux/amd64` AND `linux/arm64` platforms.

---

## Phase 5: E2E Tests on AKS

### Use the dedicated script

There is a purpose-built script for running auto-resize E2E tests on AKS:

```
hack/e2e/run-e2e-aks-autoresize.sh
```

This script handles everything: pre-flight checks, operator deployment, plugin
build, ginkgo installation, test execution, and diagnostics on failure.

### Running the tests

- [ ] First run (full pipeline — build, deploy, test):
      ```
      CONTROLLER_IMG=ghcr.io/jmealo/cloudnative-pg-testing:feat-pvc-autoresizing \
        hack/e2e/run-e2e-aks-autoresize.sh
      ```

- [ ] Subsequent runs (skip build, just re-deploy and test):
      ```
      CONTROLLER_IMG=ghcr.io/jmealo/cloudnative-pg-testing:feat-pvc-autoresizing \
        hack/e2e/run-e2e-aks-autoresize.sh --skip-build
      ```

- [ ] Re-run tests only (operator already deployed):
      ```
      CONTROLLER_IMG=ghcr.io/jmealo/cloudnative-pg-testing:feat-pvc-autoresizing \
        hack/e2e/run-e2e-aks-autoresize.sh --skip-build --skip-deploy
      ```

### Script features

The script:
- Auto-detects the default StorageClass (falls back to `managed-csi` on AKS)
- Verifies `allowVolumeExpansion: true` before starting
- Checks Azure Disk CSI driver health
- Runs tests sequentially (--nodes=1) to avoid volume attach contention
- On failure, automatically runs volume attachment diagnostics
- Reports per-test results via test-report.jq

### Diagnosis mode

If tests are timing out and you want to diagnose without re-running:
```
hack/e2e/run-e2e-aks-autoresize.sh --diagnose-only
```
This runs read-only kubectl commands to check volume attachments, pending PVCs,
stuck pods, CSI driver health, and auto-resize namespace state.

---

## KNOWN ISSUE: Azure Disk Volume Attachment Timeouts

Azure Disk CSI has a known issue where volumes take a long time to attach to
newly created pods. This causes test timeouts during cluster creation.

### Symptoms
- Pods stuck in `ContainerCreating` for 3-10+ minutes
- Events: `FailedAttachVolume` or `AttachVolume.Attach failed`
- Multiple clusters creating simultaneously exhausts Azure Disk attach queue

### Root Causes
1. **Azure Disk attach/detach queue saturation**: Each node can only process
   a limited number of disk operations concurrently. When multiple test
   clusters spin up simultaneously, attach operations queue up.
2. **WaitForFirstConsumer binding**: PVs don't provision until pods are
   scheduled, adding latency to the create-attach-mount chain.
3. **Azure API rate limiting**: Rapid create/attach operations can hit
   Azure Resource Manager throttling limits.
4. **Detach lag**: If a pod was previously on a different node, the disk
   must fully detach before re-attaching (60-90 seconds minimum).

### Diagnosis (READ-ONLY — do NOT modify the cluster)

If volume attachment timeouts occur, run these diagnostic commands:

```bash
# Check for volume attachment failures
kubectl get events --all-namespaces --field-selector reason=FailedAttachVolume \
  --sort-by='.lastTimestamp' | tail -20

# Check for mount failures
kubectl get events --all-namespaces --field-selector reason=FailedMount \
  --sort-by='.lastTimestamp' | tail -20

# Check VolumeAttachment objects (are they stuck?)
kubectl get volumeattachments \
  -o custom-columns='NAME:.metadata.name,NODE:.spec.nodeName,ATTACHED:.status.attached'

# Check pending PVCs
kubectl get pvc --all-namespaces --field-selector status.phase=Pending

# Check pods waiting for volumes
kubectl get pods --all-namespaces --field-selector status.phase=Pending \
  -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,NODE:.spec.nodeName'

# Check CSI driver pods
kubectl get pods -n kube-system -l app=csi-azuredisk-node

# Check node conditions
kubectl get nodes -o custom-columns='NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status'

# Check operator logs for autoresize-related messages
kubectl logs -n cnpg-system deployment/cnpg-controller-manager --tail=100 | grep -i 'resize\|disk\|volume'
```

Or use the script's built-in diagnostics:
```
hack/e2e/run-e2e-aks-autoresize.sh --diagnose-only
```

### VictoriaLogs queries

If the cluster has VictoriaLogs, use these queries to get all relevant logs:

```
# All logs from test namespaces and operator
{namespace=~"autoresize-.*|cnpg-system"}

# Filter for volume/resize issues
{namespace=~"autoresize-.*|cnpg-system"} AND (_msg:~"resize|volume|attach|disk|pvc")

# Azure CSI driver logs (for volume attachment issues)
{namespace="kube-system"} AND (_msg:~"csi|azuredisk|attach|detach")
```

### Mitigations in the test configuration

The E2E runner script already mitigates this by:
- Running tests sequentially (--nodes=1) to avoid parallel cluster creation
- Using 3h overall Ginkgo timeout
- Using 5-minute Eventually() timeouts for resize detection
- **Cleaning up orphaned autoresize-* namespaces** before test runs to free Azure Disks
- **Setting increased TEST_TIMEOUTS** for AKS: ClusterIsReady=900s, ClusterIsReadySlow=1200s
  (up from defaults of 600s/800s to account for Azure Disk attach latency)
- **Detecting stale VolumeAttachments** that may slow new operations

If timeouts persist despite sequential execution:
- The issue is environmental (AKS cluster health, Azure API throttling)
- Check if the cluster has sufficient node capacity
- Check if other workloads are competing for disk operations
- Report the diagnostic output to the user for triage
- Do NOT make changes to the AKS cluster — only read and report

### If tests keep failing due to volume attachment

1. Run `--diagnose-only` and capture the output
2. Check if FailedAttachVolume events mention specific error codes
3. Check if VolumeAttachment objects are stuck (ATTACHED=false for >5 min)
4. Check if CSI driver pods are restarting
5. Report findings. The user will decide whether to:
   - Wait and retry
   - Scale the cluster
   - Switch node pools
   - Try a different StorageClass

---

## Phase 6: Handle E2E Test Failures

If E2E tests fail for code reasons (not infrastructure), fix and iterate:

### Common test failures and fixes

* **Timeout waiting for PVC resize**:
  - Check reconciler is actually running: operator logs should show autoresize reconciliation
  - Check DiskStatus is populated: `kubectl get cluster -n <ns> -o json | jq '.items[0].status.diskStatus'`
  - Check the trigger threshold: is the disk actually >80% full?
  - The `dd` fill amount (1700M) may need adjustment for the actual usable volume size

* **WAL fill path wrong**:
  `kubectl exec -it <pod> -n <ns> -- ls -la /var/lib/postgresql/wal/`
  The path must match what's in the test: `/var/lib/postgresql/wal/pg_wal/fill_file`

* **Tablespace mount path wrong**:
  `kubectl exec -it <pod> -n <ns> -- ls -la /var/lib/postgresql/tablespaces/`
  CNPG mounts tablespaces at: `/var/lib/postgresql/tablespaces/<tablespace_name>/data`

* **Archive health test not blocking resize**:
  Without a backup configuration, the wal-archive command SILENTLY SUCCEEDS (no-op).
  This means pg_stat_archiver never records failures and ArchiveHealthy stays true.
  The fixture `cluster-autoresize-archive-block.yaml.template` must have a `backup`
  section with barmanObjectStore pointing to a non-existent endpoint (e.g.,
  `http://nonexistent-archive-endpoint:9000`). The test also creates a dummy Secret
  (`archive-block-dummy-creds`) with fake S3 credentials before the Cluster.
  Do NOT try to set `archive_command` directly — it's a fixed parameter and the
  webhook will reject it.

* **Archive health test — webhook rejection**:
  If the webhook rejects the cluster with `Can't set fixed configuration parameter`,
  you are trying to set a parameter from `FixedConfigurationParameters` in
  `pkg/postgres/configuration.go`. Common ones: `archive_command`, `archive_mode`,
  `listen_addresses`, `port`. Remove these from `spec.postgresql.parameters`.

* **Slot retention test not blocking**:
  pg_switch_wal() creates new WAL segments but if WAL recycling is aggressive,
  retention may be low. The test loops 10 times with 1-second sleep. If that's
  not enough to exceed `maxSlotRetentionBytes` (100MB), increase iterations or
  add data-generating SQL between switches.

* **Rate-limit test false positive**:
  The 2-minute sleep after the first resize may not be enough if the first resize
  itself took a long time. The test should compare PVC sizes, not rely purely
  on wall-clock timing. Verify the logic in the test.

* **Webhook rejection test not working**:
  Ensure the cluster object in the test has the correct spec. The webhook checks
  `!cluster.ShouldCreateWalArchiveVolume()` to determine single-volume — this
  depends on WalStorage being nil.

### Fix-commit-rebuild-test cycle

When fixing E2E issues:
1. Fix the code or fixture
2. Run `make lint && make test` locally to verify no regressions
3. Commit with DCO sign-off
4. Rebuild and push (multi-arch): `hack/e2e/run-e2e-aks-autoresize.sh` (the script builds, deploys, and tests in one step)
   OR for faster iteration: rebuild manually (see Phase 4), then `hack/e2e/run-e2e-aks-autoresize.sh --skip-build`

Phase 6 gate: All E2E tests green on AKS.

---

## Phase 7: Final Verification and Clean Up

- [ ] Final local check:
      ```
      make generate && make manifests && make fmt && make lint && make test
      ```
      All must pass.

- [ ] Squash the fix commits into logical groups (optional, if user requests):
      Only do this if the user explicitly asks for it.

Phase 7 gate: Clean git log, all local checks pass, E2E green.

---

## Commit Convention

This project requires DCO sign-off on every commit. Use the -s flag.
Include a Co-Authored-By trailer.

Format:
  git commit -s -m "$(cat <<'COMMITEOF'
  feat(component): description here

  Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>
  COMMITEOF
  )"

## Rules

- NEVER skip verification steps. Every phase gate must pass.
- NEVER commit code that doesn't compile or pass lint.
- Run `make generate && make manifests` after ANY change to api/v1/ types.
- ALWAYS use DCO sign-off: `git commit -s`.
- Only fix what is broken. Do not refactor working code.
- If stuck on the same issue for 3+ iterations, add a TODO comment and move on.
- Do NOT output <promise>COMPLETE</promise> unless ALL phases pass INCLUDING E2E on AKS.
- When diagnosing volume attachment issues, ONLY use read-only kubectl commands.
  Do NOT modify the AKS cluster, nodes, or storage configuration.

## Completion Criteria

ALL of the following must be true:
- `make generate` exits 0
- `make manifests` exits 0
- `make fmt` exits 0
- `make lint` exits 0 with no errors
- `make test` exits 0 with all tests passing
- Multi-arch Docker image (amd64+arm64) built and pushed with unique SHA-based tag
- ALL E2E tests pass on AKS via `hack/e2e/run-e2e-aks-autoresize.sh`
- All fix commits have DCO sign-off with Co-Authored-By

When ALL criteria are met, output: <promise>COMPLETE</promise>
