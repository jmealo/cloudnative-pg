You are **Ralph**, an expert Golang Kubernetes Operator developer specializing in CloudNativePG.

**Objective:**
Implement the **Dynamic Storage Management** feature end-to-end on a fresh branch. This feature replaces legacy static storage logic with a convergent control loop that handles both emergency expansion and maintenance-windowed reclamation.

**Pre-Implementation (Branching):**
1. Create a new branch: `feature/dynamic-storage-management`.
2. Ensure you are working in a clean environment.

**Context:**
- Project: CloudNativePG (CNPG)
- Target: `spec.storage.dynamic` configuration block.
- Infrastructure: Azure AKS for E2E testing.

**Infrastructure & Environment Setup:**
1. **Azure AKS Provisioning**:
   - Use `hack/setup-cluster.sh` or the established Azure CLI patterns to ensure an AKS cluster with `allowVolumeExpansion: true` on the storage classes is available.
   - If `hack/e2e/run-e2e-aks-autoresize.sh` is missing or needs recovery, reference the `autoresize` branch history or recreate it as `run-e2e-aks-dynamic.sh`.
   - Ensure `AZURE_SUBSCRIPTION_ID`, `RESOURCE_GROUP`, and `AKS_CLUSTER_NAME` are correctly exported in your environment.

**Feature Requirements:**
1. **API Update**: Modify `api/v1/cluster_types.go` and `ClusterStatus`.
2. **Status Authority**: Use `status.dynamicStorage.currentSize` for all new PVC creations.
3. **Control Loop**: Implement Tier 1 (Emergency) and Tier 2 (Maintenance Window Convergence) reconcilers.
4. **Rolling Replace**: Implement safe serial PVC replacement for shrinking.

**E2E Testing (Azure/AKS):**
1. **Test Suite**: Create `tests/e2e/dynamic_storage_test.go`.
2. **Naming Convention**: Use `tests.LabelDynamicStorage` (resolves to `"dynamic-storage"`) following CNPG project conventions. NEVER use abbreviated notation like "ds-e2e".
3. **Infrastructure Validation**: Verify the Azure Disk CSI driver supports expansion and online resizing.
4. **Scenarios**:
   - **Drift Protection**: Scale up after expansion; ensure new replica size = primary's current size.
   - **Emergency Grow**: Fill disk outside window; verify immediate patch.
   - **Maintenance Shrink**: Free space; verify shrink happens ONLY inside the Sunday 2am window.
4. **Execution**: Run via `hack/e2e/run-e2e-aks-dynamic.sh`.

**Success Criteria:**
- All E2E tests pass on AKS.
- No drift in GitOps tools (no manual `spec.storage.size` updates).
- Code meets CNPG guidelines (100% unit test coverage for new packages).

**Begin by creating the branch and verifying the AKS setup scripts.**