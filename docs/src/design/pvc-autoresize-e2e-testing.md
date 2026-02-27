# PVC Auto-Resize E2E Testing Design

| Field | Value |
|-------|-------|
| **Status** | Draft |
| **Author** | Jeff Mealo |
| **Created** | 2026-02-06 |
| **Parent Document** | [PVC Auto-Resize Design](pvc-autoresize.md) |

## Table of Contents

1. [Overview](#overview)
2. [Testing Challenges](#testing-challenges)
3. [Test Infrastructure Requirements](#test-infrastructure-requirements)
4. [Test Fixtures](#test-fixtures)
5. [Helper Functions](#helper-functions)
6. [Test Scenarios](#test-scenarios)
7. [Metrics Verification](#metrics-verification)
8. [Test Execution](#test-execution)
9. [CI/CD Considerations](#cicd-considerations)

---

## Overview

This document describes the E2E testing strategy for the PVC auto-resize feature. The tests follow the existing CNPG E2E testing patterns using Ginkgo/Gomega and integrate with the established test infrastructure.

### Key Testing Objectives

1. **Threshold-triggered resize** - Verify PVCs expand when usage exceeds threshold
2. **WAL safety enforcement** - Verify resize blocks when archive/slots unhealthy
3. **Configuration validation** - Verify webhook rejects invalid configurations
4. **Metrics accuracy** - Verify disk metrics from `statfs` are exposed correctly
5. **Safety mechanisms** - Verify cooldown, maxSize, and single-volume acknowledgment

---

## Testing Challenges

### Challenge 1: Simulating Disk Fill

To trigger auto-resize, we need to fill disk to threshold percentage. Options:

| Approach | Pros | Cons |
|----------|------|------|
| `dd if=/dev/zero` | Fast, predictable | Requires exec into pod |
| `pg_create_table` with data | Uses PostgreSQL normally | Slower, unpredictable WAL |
| Pre-sized PVC (small) | Quick threshold breach | Limited to small volumes |

**Recommendation:** Use small initial PVCs (500Mi-1Gi) combined with `dd` to quickly fill to threshold. This mirrors existing `disk_space_test.go` patterns.

### Challenge 2: Simulating Archive Failures

To test WAL safety blocks, we need archive failures. Options:

| Approach | Pros | Cons |
|----------|------|------|
| Invalid S3 credentials | Realistic | Requires object storage |
| Non-existent barman endpoint | Simple | Needs backup config |
| `archive_command = '/bin/false'` | Direct control | Modifies pg config |
| Block network to backup target | Realistic | Complex network policy |

**Recommendation:** Use a deliberately misconfigured backup destination (invalid credentials or non-existent endpoint) to trigger archive failures. The `pg_stat_archiver.failed_count` will increase and `archive_status/*.ready` files will accumulate.

### Challenge 3: Simulating Inactive Replication Slots

To test slot retention blocking, we need inactive slots. Options:

| Approach | Pros | Cons |
|----------|------|------|
| Create slot, don't consume | Direct control | Simple |
| Disable replica, keep slot | Realistic | Affects cluster health |
| Scale down with slots enabled | Mirrors production issues | Destructive |

**Recommendation:** Create a physical replication slot via SQL and never consume it. This accumulates WAL retention.

### Challenge 4: Timing and CSI Behavior

PVC expansion timing varies by CSI driver:

- **Online expansion** (most CSI drivers): Immediate filesystem resize
- **Offline expansion**: Requires pod restart
- **CSI controller delays**: Variable time for volume resize

**Recommendation:** Use generous `Eventually` timeouts (300-600s) with 5s polling intervals. Skip tests on storage classes that don't support expansion.

### Challenge 5: Verifying Actual vs Requested Size

The design calls for detecting CSI failures by comparing actual (statfs) vs requested (PVC spec) size.

**Recommendation:** After resize, verify:
1. PVC `.spec.resources.requests.storage` shows new size
2. PVC `.status.capacity.storage` shows new size
3. Instance metrics show new `cnpg_disk_total_bytes`

---

## Test Infrastructure Requirements

### Storage Class Requirements

```yaml
# Required for auto-resize tests
allowVolumeExpansion: true
```

Tests should skip with clear message if storage class doesn't support expansion (matches existing `storage_expansion_test.go` pattern).

### Environment Variables

| Variable | Purpose | Default |
|----------|---------|---------|
| `E2E_DEFAULT_STORAGE_CLASS` | Storage class to use | Required |
| `E2E_BACKUP_OBJECT_STORE_*` | Backup destination for archive tests | Optional |

### New Test Labels

```go
// tests/labels.go additions
LabelAutoResize = "autoresize"  // All auto-resize tests
```

### Test Level

Auto-resize tests should be `tests.Medium` level - they require specific infrastructure but aren't as resource-intensive as backup/restore tests.

---

## Test Fixtures

### Directory Structure

```
tests/e2e/fixtures/pvc_autoresize/
├── cluster-autoresize-basic.yaml.template
├── cluster-autoresize-separate-wal.yaml.template
├── cluster-autoresize-single-volume.yaml.template
├── cluster-autoresize-single-volume-no-ack.yaml.template
├── cluster-autoresize-with-backup.yaml.template
├── cluster-autoresize-with-tablespace.yaml.template
├── cluster-autoresize-maxsize.yaml.template
└── cluster-autoresize-cooldown.yaml.template
```

### Basic Auto-Resize Fixture

```yaml
# cluster-autoresize-basic.yaml.template
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: autoresize-basic
spec:
  instances: 3

  storage:
    size: 500Mi
    storageClass: ${E2E_DEFAULT_STORAGE_CLASS}
    autoResize:
      enabled: true
      threshold: 70
      increase: "200Mi"
      maxSize: "2Gi"
      cooldownPeriod: 30s  # Short for testing

  walStorage:
    size: 200Mi
    storageClass: ${E2E_DEFAULT_STORAGE_CLASS}
    autoResize:
      enabled: true
      threshold: 60
      increase: "100Mi"
      maxSize: "1Gi"
      walSafetyPolicy:
        requireArchiveHealthy: true
        maxPendingWALFiles: 10
```

### Single Volume (No Acknowledgment) Fixture

```yaml
# cluster-autoresize-single-volume-no-ack.yaml.template
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: autoresize-single-no-ack
spec:
  instances: 1

  storage:
    size: 500Mi
    storageClass: ${E2E_DEFAULT_STORAGE_CLASS}
    autoResize:
      enabled: true
      threshold: 70
      # Missing walSafetyPolicy.acknowledgeWALRisk - should be rejected
```

### Single Volume (With Acknowledgment) Fixture

```yaml
# cluster-autoresize-single-volume.yaml.template
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: autoresize-single-ack
spec:
  instances: 1

  storage:
    size: 500Mi
    storageClass: ${E2E_DEFAULT_STORAGE_CLASS}
    autoResize:
      enabled: true
      threshold: 70
      increase: "200Mi"
      maxSize: "2Gi"
      walSafetyPolicy:
        acknowledgeWALRisk: true
        requireArchiveHealthy: true
```

### With Backup Configuration Fixture

```yaml
# cluster-autoresize-with-backup.yaml.template
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: autoresize-with-backup
spec:
  instances: 3

  storage:
    size: 500Mi
    storageClass: ${E2E_DEFAULT_STORAGE_CLASS}
    autoResize:
      enabled: true
      threshold: 70
      increase: "200Mi"

  walStorage:
    size: 200Mi
    storageClass: ${E2E_DEFAULT_STORAGE_CLASS}
    autoResize:
      enabled: true
      threshold: 60
      increase: "100Mi"
      walSafetyPolicy:
        requireArchiveHealthy: true
        maxPendingWALFiles: 5

  backup:
    barmanObjectStore:
      destinationPath: ${BACKUP_DESTINATION_PATH}
      endpointURL: ${BACKUP_ENDPOINT_URL}
      s3Credentials:
        accessKeyId:
          name: backup-creds
          key: ACCESS_KEY_ID
        secretAccessKey:
          name: backup-creds
          key: SECRET_ACCESS_KEY
```

---

## Helper Functions

### New Assertions for asserts_test.go

```go
// FillDiskToPercentage fills the disk at the given path to target percentage
// Returns the path to the fill file for cleanup
func FillDiskToPercentage(namespace, podName, volumePath string, targetPercent int) string {
    By(fmt.Sprintf("filling %s to %d%% on pod %s", volumePath, targetPercent, podName))

    // Get current disk stats
    stdout, _, err := exec.CommandInInstancePod(
        env.Ctx, env.Client, env.Interface, env.RestClientConfig,
        exec.PodLocator{Namespace: namespace, PodName: podName},
        nil,
        "df", "-B1", volumePath,
    )
    Expect(err).ToNot(HaveOccurred())

    // Parse df output to get current used/total
    // Format: Filesystem 1B-blocks Used Available Use% Mounted
    lines := strings.Split(strings.TrimSpace(stdout), "\n")
    Expect(len(lines)).To(BeNumerically(">=", 2))
    fields := strings.Fields(lines[1])
    total, err := strconv.ParseInt(fields[1], 10, 64)
    Expect(err).ToNot(HaveOccurred())
    used, err := strconv.ParseInt(fields[2], 10, 64)
    Expect(err).ToNot(HaveOccurred())

    // Calculate bytes needed to reach target
    targetUsed := (total * int64(targetPercent)) / 100
    bytesToWrite := targetUsed - used
    if bytesToWrite <= 0 {
        GinkgoWriter.Printf("Disk already at %d%%, target is %d%%\n",
            (used*100)/total, targetPercent)
        return ""
    }

    // Create fill file
    fillPath := filepath.Join(volumePath, "autoresize-fill-file")
    mbToWrite := bytesToWrite / (1024 * 1024)
    if mbToWrite < 1 {
        mbToWrite = 1
    }

    timeout := time.Minute * 5
    _, _, err = exec.CommandInInstancePod(
        env.Ctx, env.Client, env.Interface, env.RestClientConfig,
        exec.PodLocator{Namespace: namespace, PodName: podName},
        &timeout,
        "dd", "if=/dev/zero", fmt.Sprintf("of=%s", fillPath),
        "bs=1M", fmt.Sprintf("count=%d", mbToWrite),
    )
    // dd may error if disk fills completely - that's expected
    if err != nil {
        GinkgoWriter.Printf("dd command returned error (may be expected): %v\n", err)
    }

    return fillPath
}

// CleanupFillFile removes a file created by FillDiskToPercentage
func CleanupFillFile(namespace, podName, fillPath string) {
    if fillPath == "" {
        return
    }
    By(fmt.Sprintf("cleaning up fill file %s", fillPath))
    _, _, _ = exec.CommandInInstancePod(
        env.Ctx, env.Client, env.Interface, env.RestClientConfig,
        exec.PodLocator{Namespace: namespace, PodName: podName},
        nil,
        "rm", "-f", fillPath,
    )
}

// AssertAutoResizeTriggered verifies that PVC was automatically resized
func AssertAutoResizeTriggered(namespace, clusterName, pvcName string, expectedMinSize resource.Quantity) {
    By(fmt.Sprintf("verifying PVC %s was auto-resized to at least %s", pvcName, expectedMinSize.String()))

    Eventually(func(g Gomega) {
        pvc := &corev1.PersistentVolumeClaim{}
        err := env.Client.Get(env.Ctx, types.NamespacedName{
            Namespace: namespace,
            Name:      pvcName,
        }, pvc)
        g.Expect(err).ToNot(HaveOccurred())

        // Check spec (requested size)
        requestedSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
        g.Expect(requestedSize.Cmp(expectedMinSize)).To(BeNumerically(">=", 0),
            "requested size %s should be >= %s", requestedSize.String(), expectedMinSize.String())

        // Check status (actual capacity after CSI expansion)
        actualCapacity := pvc.Status.Capacity[corev1.ResourceStorage]
        g.Expect(actualCapacity.Cmp(expectedMinSize)).To(BeNumerically(">=", 0),
            "actual capacity %s should be >= %s", actualCapacity.String(), expectedMinSize.String())

    }, 300, 5).Should(Succeed())
}

// AssertAutoResizeBlocked verifies that auto-resize was blocked
func AssertAutoResizeBlocked(namespace, clusterName, expectedReason string) {
    By(fmt.Sprintf("verifying auto-resize is blocked with reason: %s", expectedReason))

    Eventually(func(g Gomega) {
        cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
        g.Expect(err).ToNot(HaveOccurred())

        // Check for AutoResizeBlocked condition
        for _, cond := range cluster.Status.Conditions {
            if cond.Type == "AutoResizeBlocked" && cond.Status == metav1.ConditionTrue {
                g.Expect(cond.Message).To(ContainSubstring(expectedReason))
                return
            }
        }
        g.Expect(false).To(BeTrue(), "AutoResizeBlocked condition not found")
    }, 120, 5).Should(Succeed())
}

// AssertAutoResizeNotBlocked verifies no auto-resize blocking condition exists
func AssertAutoResizeNotBlocked(namespace, clusterName string) {
    By("verifying auto-resize is not blocked")

    Consistently(func(g Gomega) {
        cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
        g.Expect(err).ToNot(HaveOccurred())

        for _, cond := range cluster.Status.Conditions {
            if cond.Type == "AutoResizeBlocked" && cond.Status == metav1.ConditionTrue {
                g.Expect(false).To(BeTrue(), "unexpected AutoResizeBlocked condition: %s", cond.Message)
            }
        }
    }, 30, 5).Should(Succeed())
}

// AssertDiskMetricsExist verifies disk metrics are exposed on instance
func AssertDiskMetricsExist(namespace, clusterName string, volumeTypes []string) {
    By("verifying disk metrics are exposed")

    podList, err := clusterutils.ListPods(env.Ctx, env.Client, namespace, clusterName)
    Expect(err).ToNot(HaveOccurred())

    cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
    Expect(err).ToNot(HaveOccurred())

    for _, pod := range podList.Items {
        out, err := proxy.RetrieveMetricsFromInstance(env.Ctx, env.Interface, pod,
            cluster.IsMetricsTLSEnabled())
        Expect(err).ToNot(HaveOccurred())

        for _, volType := range volumeTypes {
            expectedMetrics := map[string]*regexp.Regexp{
                fmt.Sprintf(`cnpg_disk_total_bytes{.*volume_type="%s".*}`, volType):     regexp.MustCompile(`\d+`),
                fmt.Sprintf(`cnpg_disk_used_bytes{.*volume_type="%s".*}`, volType):      regexp.MustCompile(`\d+`),
                fmt.Sprintf(`cnpg_disk_available_bytes{.*volume_type="%s".*}`, volType): regexp.MustCompile(`\d+`),
                fmt.Sprintf(`cnpg_disk_percent_used{.*volume_type="%s".*}`, volType):    regexp.MustCompile(`[\d.]+`),
            }
            assertIncludesMetrics(out, expectedMetrics)
        }
    }
}

// AssertDiskMetricValues verifies specific disk metric values
func AssertDiskMetricValues(namespace, podName string, volumeType string, minTotalBytes, maxUsedPercent int64) {
    By(fmt.Sprintf("verifying disk metric values for %s volume", volumeType))

    cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
    Expect(err).ToNot(HaveOccurred())

    pod, err := pods.Get(env.Ctx, env.Client, namespace, podName)
    Expect(err).ToNot(HaveOccurred())

    out, err := proxy.RetrieveMetricsFromInstance(env.Ctx, env.Interface, *pod,
        cluster.IsMetricsTLSEnabled())
    Expect(err).ToNot(HaveOccurred())

    // Parse metric values
    totalBytesRegex := regexp.MustCompile(fmt.Sprintf(`cnpg_disk_total_bytes{[^}]*volume_type="%s"[^}]*}\s+(\d+)`, volumeType))
    matches := totalBytesRegex.FindStringSubmatch(out)
    Expect(len(matches)).To(BeNumerically(">=", 2))
    totalBytes, err := strconv.ParseInt(matches[1], 10, 64)
    Expect(err).ToNot(HaveOccurred())
    Expect(totalBytes).To(BeNumerically(">=", minTotalBytes))
}

// CountPendingArchiveFiles counts .ready files in archive_status
func CountPendingArchiveFiles(namespace, podName string) int {
    stdout, _, err := exec.CommandInInstancePod(
        env.Ctx, env.Client, env.Interface, env.RestClientConfig,
        exec.PodLocator{Namespace: namespace, PodName: podName},
        nil,
        "sh", "-c", "ls /var/lib/postgresql/data/pgdata/pg_wal/archive_status/*.ready 2>/dev/null | wc -l",
    )
    if err != nil {
        return 0
    }
    count, err := strconv.Atoi(strings.TrimSpace(stdout))
    if err != nil {
        return 0
    }
    return count
}

// CreateInactiveReplicationSlot creates a physical slot that won't be consumed
func CreateInactiveReplicationSlot(namespace, podName, slotName string) {
    By(fmt.Sprintf("creating inactive replication slot: %s", slotName))

    query := fmt.Sprintf("SELECT pg_create_physical_replication_slot('%s')", slotName)
    _, _, err := exec.QueryInInstancePod(
        env.Ctx, env.Client, env.Interface, env.RestClientConfig,
        exec.PodLocator{Namespace: namespace, PodName: podName},
        postgres.PostgresDBName,
        query)
    Expect(err).ToNot(HaveOccurred())
}

// DropReplicationSlot drops a replication slot
func DropReplicationSlot(namespace, podName, slotName string) {
    By(fmt.Sprintf("dropping replication slot: %s", slotName))

    query := fmt.Sprintf("SELECT pg_drop_replication_slot('%s')", slotName)
    _, _, err := exec.QueryInInstancePod(
        env.Ctx, env.Client, env.Interface, env.RestClientConfig,
        exec.PodLocator{Namespace: namespace, PodName: podName},
        postgres.PostgresDBName,
        query)
    // Ignore error - slot may not exist
    _ = err
}

// GenerateWAL generates WAL by forcing checkpoints and WAL switches
func GenerateWAL(namespace, podName string, iterations int) {
    By(fmt.Sprintf("generating WAL (%d iterations)", iterations))

    for i := 0; i < iterations; i++ {
        // Create table with data, checkpoint, switch WAL
        queries := []string{
            fmt.Sprintf("CREATE TABLE IF NOT EXISTS wal_gen_%d (id serial, data text)", i),
            fmt.Sprintf("INSERT INTO wal_gen_%d (data) SELECT md5(random()::text) FROM generate_series(1, 10000)", i),
            "CHECKPOINT",
            "SELECT pg_switch_wal()",
        }
        for _, query := range queries {
            _, _, err := exec.QueryInInstancePod(
                env.Ctx, env.Client, env.Interface, env.RestClientConfig,
                exec.PodLocator{Namespace: namespace, PodName: podName},
                postgres.PostgresDBName,
                query)
            if err != nil {
                GinkgoWriter.Printf("WAL generation query error (may be expected): %v\n", err)
            }
        }
    }
}

// GetClusterAutoResizeEvent returns the last auto-resize event from cluster status
func GetClusterAutoResizeEvent(namespace, clusterName string) *apiv1.AutoResizeEvent {
    cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
    Expect(err).ToNot(HaveOccurred())

    if cluster.Status.DiskStatus == nil {
        return nil
    }
    return cluster.Status.DiskStatus.LastAutoResize
}
```

---

## Test Scenarios

### File: `tests/e2e/pvc_autoresize_test.go`

```go
/*
Copyright © contributors to CloudNativePG, established as
CloudNativePG a Series of LF Projects, LLC.
... (standard header)
*/

package e2e

import (
    "fmt"
    "os"
    "time"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/api/resource"
    "k8s.io/apimachinery/pkg/types"
    ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

    apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
    "github.com/cloudnative-pg/cloudnative-pg/tests"
    "github.com/cloudnative-pg/cloudnative-pg/tests/utils/clusterutils"
    "github.com/cloudnative-pg/cloudnative-pg/tests/utils/storage"
    "github.com/cloudnative-pg/cloudnative-pg/tests/utils/yaml"

    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"
)

var _ = Describe("PVC Auto-Resize", Label(tests.LabelStorage, tests.LabelAutoResize), func() {
    const (
        fixtureDir      = fixturesDir + "/pvc_autoresize"
        level           = tests.Medium
        namespacePrefix = "autoresize-e2e"
    )

    var namespace string

    BeforeEach(func() {
        if testLevelEnv.Depth < int(level) {
            Skip("Test depth is lower than the amount requested for this test")
        }

        // Check storage class supports expansion
        storageClass := os.Getenv("E2E_DEFAULT_STORAGE_CLASS")
        allowExpansion, err := storage.GetStorageAllowExpansion(
            env.Ctx, env.Client, storageClass,
        )
        Expect(err).ToNot(HaveOccurred())
        if allowExpansion == nil || !*allowExpansion {
            Skip(fmt.Sprintf("Storage class %s does not support volume expansion", storageClass))
        }
    })

    // ========================================
    // BASIC AUTO-RESIZE TESTS
    // ========================================

    Context("Basic auto-resize functionality", func() {
        const (
            sampleFile  = fixtureDir + "/cluster-autoresize-basic.yaml.template"
            clusterName = "autoresize-basic"
        )

        It("resizes data PVC when threshold is exceeded", func() {
            var err error
            namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix+"-basic")
            Expect(err).ToNot(HaveOccurred())

            By("creating cluster with auto-resize enabled")
            AssertCreateCluster(namespace, clusterName, sampleFile, env)

            By("verifying disk metrics are exposed")
            AssertDiskMetricsExist(namespace, clusterName, []string{"data", "wal"})

            By("getting primary pod")
            primary, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
            Expect(err).ToNot(HaveOccurred())

            By("filling data volume to threshold")
            fillPath := FillDiskToPercentage(namespace, primary.Name, "/var/lib/postgresql/data", 75)
            defer CleanupFillFile(namespace, primary.Name, fillPath)

            By("verifying auto-resize was triggered")
            expectedMinSize := resource.MustParse("700Mi") // 500Mi + 200Mi increase
            AssertAutoResizeTriggered(namespace, clusterName, primary.Name, expectedMinSize)

            By("verifying auto-resize event was recorded")
            Eventually(func(g Gomega) {
                event := GetClusterAutoResizeEvent(namespace, clusterName)
                g.Expect(event).ToNot(BeNil())
                g.Expect(event.VolumeType).To(Equal("data"))
                g.Expect(event.PodName).To(Equal(primary.Name))
            }, 60, 5).Should(Succeed())
        })

        It("resizes WAL PVC when threshold is exceeded", func() {
            var err error
            namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix+"-wal")
            Expect(err).ToNot(HaveOccurred())

            By("creating cluster with auto-resize enabled")
            AssertCreateCluster(namespace, clusterName, sampleFile, env)

            By("getting primary pod")
            primary, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
            Expect(err).ToNot(HaveOccurred())

            By("filling WAL volume to threshold")
            fillPath := FillDiskToPercentage(namespace, primary.Name, "/var/lib/postgresql/wal", 65)
            defer CleanupFillFile(namespace, primary.Name, fillPath)

            By("verifying WAL PVC auto-resize was triggered")
            walPVCName := primary.Name + "-wal"
            expectedMinSize := resource.MustParse("300Mi") // 200Mi + 100Mi increase
            AssertAutoResizeTriggered(namespace, clusterName, walPVCName, expectedMinSize)
        })
    })

    // ========================================
    // WAL SAFETY TESTS
    // ========================================

    Context("WAL safety mechanisms", Ordered, func() {
        const (
            sampleFile  = fixtureDir + "/cluster-autoresize-with-backup.yaml.template"
            clusterName = "autoresize-wal-safety"
        )

        BeforeAll(func() {
            // Skip if no backup configuration available
            if os.Getenv("BACKUP_DESTINATION_PATH") == "" {
                Skip("Backup configuration not available for WAL safety tests")
            }
        })

        It("blocks resize when archive is unhealthy", func() {
            var err error
            namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix+"-archive")
            Expect(err).ToNot(HaveOccurred())

            By("creating cluster with invalid backup credentials")
            // Use deliberately invalid credentials to cause archive failures
            AssertCreateCluster(namespace, clusterName, sampleFile, env)

            By("getting primary pod")
            primary, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
            Expect(err).ToNot(HaveOccurred())

            By("generating WAL to trigger archive attempts")
            GenerateWAL(namespace, primary.Name, 20)

            By("waiting for pending archive files to accumulate")
            Eventually(func() int {
                return CountPendingArchiveFiles(namespace, primary.Name)
            }, 120, 5).Should(BeNumerically(">", 5))

            By("filling WAL volume to threshold")
            fillPath := FillDiskToPercentage(namespace, primary.Name, "/var/lib/postgresql/wal", 65)
            defer CleanupFillFile(namespace, primary.Name, fillPath)

            By("verifying auto-resize is blocked due to archive health")
            AssertAutoResizeBlocked(namespace, clusterName, "archive")
        })

        It("blocks resize when inactive replication slots exist", func() {
            var err error
            namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix+"-slots")
            Expect(err).ToNot(HaveOccurred())

            By("creating cluster")
            AssertCreateCluster(namespace, clusterName, sampleFile, env)

            By("getting primary pod")
            primary, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
            Expect(err).ToNot(HaveOccurred())

            By("creating inactive replication slot")
            CreateInactiveReplicationSlot(namespace, primary.Name, "stuck_slot")
            defer DropReplicationSlot(namespace, primary.Name, "stuck_slot")

            By("generating WAL to increase slot retention")
            GenerateWAL(namespace, primary.Name, 50)

            By("filling WAL volume to threshold")
            fillPath := FillDiskToPercentage(namespace, primary.Name, "/var/lib/postgresql/wal", 65)
            defer CleanupFillFile(namespace, primary.Name, fillPath)

            By("verifying auto-resize is blocked due to inactive slots")
            AssertAutoResizeBlocked(namespace, clusterName, "inactive")
        })
    })

    // ========================================
    // SINGLE VOLUME TESTS
    // ========================================

    Context("Single volume clusters", func() {
        It("requires acknowledgeWALRisk for auto-resize", func() {
            sampleFile := fixtureDir + "/cluster-autoresize-single-volume-no-ack.yaml.template"
            clusterName := "autoresize-single-no-ack"

            var err error
            namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix+"-single-noack")
            Expect(err).ToNot(HaveOccurred())

            By("attempting to create cluster without acknowledgeWALRisk")
            _, err = yaml.GetResourceNameFromYAML(env.Scheme, sampleFile)
            Expect(err).ToNot(HaveOccurred())

            // Create should fail with webhook rejection
            cmd := fmt.Sprintf("kubectl apply -n %s -f %s", namespace, sampleFile)
            _, stderr, err := run.Unchecked(cmd)
            Expect(err).To(HaveOccurred())
            Expect(stderr).To(ContainSubstring("acknowledgeWALRisk"))
        })

        It("allows auto-resize when acknowledgeWALRisk is true", func() {
            sampleFile := fixtureDir + "/cluster-autoresize-single-volume.yaml.template"
            clusterName := "autoresize-single-ack"

            var err error
            namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix+"-single-ack")
            Expect(err).ToNot(HaveOccurred())

            By("creating cluster with acknowledgeWALRisk: true")
            AssertCreateCluster(namespace, clusterName, sampleFile, env)

            By("getting primary pod")
            primary, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
            Expect(err).ToNot(HaveOccurred())

            By("filling volume to threshold")
            fillPath := FillDiskToPercentage(namespace, primary.Name, "/var/lib/postgresql/data", 75)
            defer CleanupFillFile(namespace, primary.Name, fillPath)

            By("verifying auto-resize was triggered")
            expectedMinSize := resource.MustParse("700Mi")
            AssertAutoResizeTriggered(namespace, clusterName, primary.Name, expectedMinSize)
        })
    })

    // ========================================
    // SAFETY MECHANISM TESTS
    // ========================================

    Context("Safety mechanisms", func() {
        It("respects maxSize limit", func() {
            sampleFile := fixtureDir + "/cluster-autoresize-maxsize.yaml.template"
            clusterName := "autoresize-maxsize"

            var err error
            namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix+"-maxsize")
            Expect(err).ToNot(HaveOccurred())

            By("creating cluster with small maxSize")
            AssertCreateCluster(namespace, clusterName, sampleFile, env)

            By("getting primary pod")
            primary, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
            Expect(err).ToNot(HaveOccurred())

            By("triggering multiple resize operations")
            // Fill to trigger resize multiple times
            for i := 0; i < 3; i++ {
                fillPath := FillDiskToPercentage(namespace, primary.Name, "/var/lib/postgresql/data", 75)
                // Wait for resize or maxSize reached
                time.Sleep(45 * time.Second)
                CleanupFillFile(namespace, primary.Name, fillPath)
            }

            By("verifying maxSize was respected")
            pvc := &corev1.PersistentVolumeClaim{}
            pvcName := types.NamespacedName{Namespace: namespace, Name: primary.Name}
            err = env.Client.Get(env.Ctx, pvcName, pvc)
            Expect(err).ToNot(HaveOccurred())

            maxSize := resource.MustParse("1Gi")
            actualSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
            Expect(actualSize.Cmp(maxSize)).To(BeNumerically("<=", 0))
        })

        It("respects cooldown period", func() {
            sampleFile := fixtureDir + "/cluster-autoresize-cooldown.yaml.template"
            clusterName := "autoresize-cooldown"

            var err error
            namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix+"-cooldown")
            Expect(err).ToNot(HaveOccurred())

            By("creating cluster with cooldown period")
            AssertCreateCluster(namespace, clusterName, sampleFile, env)

            By("getting primary pod")
            primary, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
            Expect(err).ToNot(HaveOccurred())

            By("triggering first resize")
            fillPath := FillDiskToPercentage(namespace, primary.Name, "/var/lib/postgresql/data", 75)
            expectedMinSize := resource.MustParse("700Mi")
            AssertAutoResizeTriggered(namespace, clusterName, primary.Name, expectedMinSize)
            CleanupFillFile(namespace, primary.Name, fillPath)

            By("recording resize event time")
            event := GetClusterAutoResizeEvent(namespace, clusterName)
            Expect(event).ToNot(BeNil())
            firstResizeTime := event.Time.Time

            By("immediately triggering threshold again")
            fillPath = FillDiskToPercentage(namespace, primary.Name, "/var/lib/postgresql/data", 75)

            By("verifying second resize is blocked by cooldown")
            Consistently(func(g Gomega) {
                pvc := &corev1.PersistentVolumeClaim{}
                pvcName := types.NamespacedName{Namespace: namespace, Name: primary.Name}
                g.Expect(env.Client.Get(env.Ctx, pvcName, pvc)).ToNot(HaveOccurred())

                // Size should not have increased beyond first resize
                size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
                g.Expect(size.Cmp(expectedMinSize)).To(BeNumerically("==", 0))
            }, 30, 5).Should(Succeed())

            CleanupFillFile(namespace, primary.Name, fillPath)

            By("waiting for cooldown to expire")
            // Fixture uses 60s cooldown
            time.Sleep(70 * time.Second)

            By("verifying resize works after cooldown")
            fillPath = FillDiskToPercentage(namespace, primary.Name, "/var/lib/postgresql/data", 75)
            defer CleanupFillFile(namespace, primary.Name, fillPath)

            secondExpectedSize := resource.MustParse("900Mi") // 700Mi + 200Mi
            AssertAutoResizeTriggered(namespace, clusterName, primary.Name, secondExpectedSize)

            // Verify new event recorded
            event = GetClusterAutoResizeEvent(namespace, clusterName)
            Expect(event).ToNot(BeNil())
            Expect(event.Time.Time.After(firstResizeTime)).To(BeTrue())
        })
    })

    // ========================================
    // METRICS TESTS
    // ========================================

    Context("Metrics verification", func() {
        const (
            sampleFile  = fixtureDir + "/cluster-autoresize-basic.yaml.template"
            clusterName = "autoresize-metrics"
        )

        It("exposes accurate disk metrics via Prometheus", func() {
            var err error
            namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix+"-metrics")
            Expect(err).ToNot(HaveOccurred())

            By("creating cluster")
            AssertCreateCluster(namespace, clusterName, sampleFile, env)

            By("verifying disk metrics exist on all pods")
            AssertDiskMetricsExist(namespace, clusterName, []string{"data", "wal"})

            By("verifying metric values are reasonable")
            // Total bytes should be at least 400MB (500Mi with filesystem overhead)
            primary, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
            Expect(err).ToNot(HaveOccurred())
            AssertDiskMetricValues(namespace, primary.Name, "data", 400*1024*1024, 50)
        })

        It("updates resize_blocked metric when resize is blocked", func() {
            // This test requires backup configuration
            if os.Getenv("BACKUP_DESTINATION_PATH") == "" {
                Skip("Backup configuration not available")
            }

            sampleFile := fixtureDir + "/cluster-autoresize-with-backup.yaml.template"
            clusterName := "autoresize-blocked-metric"

            var err error
            namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix+"-blocked")
            Expect(err).ToNot(HaveOccurred())

            By("creating cluster with failing archive")
            AssertCreateCluster(namespace, clusterName, sampleFile, env)

            By("getting primary and generating WAL")
            primary, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
            Expect(err).ToNot(HaveOccurred())
            GenerateWAL(namespace, primary.Name, 20)

            By("filling volume to trigger resize evaluation")
            fillPath := FillDiskToPercentage(namespace, primary.Name, "/var/lib/postgresql/wal", 65)
            defer CleanupFillFile(namespace, primary.Name, fillPath)

            By("verifying resize_blocked metric is set")
            Eventually(func(g Gomega) {
                cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
                g.Expect(err).ToNot(HaveOccurred())

                out, err := proxy.RetrieveMetricsFromInstance(env.Ctx, env.Interface, *primary,
                    cluster.IsMetricsTLSEnabled())
                g.Expect(err).ToNot(HaveOccurred())

                expectedMetrics := map[string]*regexp.Regexp{
                    `cnpg_disk_resize_blocked{.*volume_type="wal".*reason="archive_unhealthy".*}`: regexp.MustCompile(`1`),
                }
                assertIncludesMetrics(out, expectedMetrics)
            }, 120, 5).Should(Succeed())
        })
    })

    // ========================================
    // TABLESPACE TESTS
    // ========================================

    Context("Tablespace auto-resize", Label(tests.LabelTablespaces), func() {
        const (
            sampleFile  = fixtureDir + "/cluster-autoresize-with-tablespace.yaml.template"
            clusterName = "autoresize-tablespace"
        )

        It("resizes tablespace PVC when threshold is exceeded", func() {
            var err error
            namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix+"-tbs")
            Expect(err).ToNot(HaveOccurred())

            By("creating cluster with tablespace")
            AssertCreateCluster(namespace, clusterName, sampleFile, env)

            By("getting primary pod")
            primary, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
            Expect(err).ToNot(HaveOccurred())

            By("filling tablespace volume to threshold")
            tbsPath := "/var/lib/postgresql/tablespaces/large_objects"
            fillPath := FillDiskToPercentage(namespace, primary.Name, tbsPath, 75)
            defer CleanupFillFile(namespace, primary.Name, fillPath)

            By("verifying tablespace PVC auto-resize was triggered")
            tbsPVCName := primary.Name + "-tbs-large_objects"
            expectedMinSize := resource.MustParse("700Mi")
            AssertAutoResizeTriggered(namespace, clusterName, tbsPVCName, expectedMinSize)

            By("verifying tablespace metrics exist")
            AssertDiskMetricsExist(namespace, clusterName, []string{"tablespace"})
        })
    })
})
```

---

## Metrics Verification

### Expected Metrics After Auto-Resize

```promql
# After successful resize, verify:

# Total capacity increased
cnpg_disk_total_bytes{volume_type="data"} > previous_value

# Used percentage decreased (after resize)
cnpg_disk_percent_used{volume_type="data"} < threshold

# Resize counter incremented
cnpg_disk_resizes_total{result="success"} > 0

# No blocked status
cnpg_disk_resize_blocked == 0

# At max size (when limit reached)
cnpg_disk_at_max_size{volume_type="data"} == 1
```

### Metrics Test Helper

```go
// AssertAutoResizeMetrics verifies all auto-resize related metrics
func AssertAutoResizeMetrics(namespace, clusterName, podName, volumeType string, resized bool) {
    cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
    Expect(err).ToNot(HaveOccurred())

    pod, err := pods.Get(env.Ctx, env.Client, namespace, podName)
    Expect(err).ToNot(HaveOccurred())

    out, err := proxy.RetrieveMetricsFromInstance(env.Ctx, env.Interface, *pod,
        cluster.IsMetricsTLSEnabled())
    Expect(err).ToNot(HaveOccurred())

    expectedMetrics := map[string]*regexp.Regexp{
        // Core disk metrics
        fmt.Sprintf(`cnpg_disk_total_bytes{.*volume_type="%s".*}`, volumeType):     regexp.MustCompile(`\d+`),
        fmt.Sprintf(`cnpg_disk_used_bytes{.*volume_type="%s".*}`, volumeType):      regexp.MustCompile(`\d+`),
        fmt.Sprintf(`cnpg_disk_available_bytes{.*volume_type="%s".*}`, volumeType): regexp.MustCompile(`\d+`),
        fmt.Sprintf(`cnpg_disk_percent_used{.*volume_type="%s".*}`, volumeType):    regexp.MustCompile(`[\d.]+`),
    }

    if resized {
        expectedMetrics[fmt.Sprintf(`cnpg_disk_resizes_total{.*volume_type="%s".*result="success".*}`, volumeType)] = regexp.MustCompile(`[1-9]\d*`)
    }

    assertIncludesMetrics(out, expectedMetrics)
}
```

---

## Test Execution

### Running Auto-Resize Tests

```bash
# Run all auto-resize tests
make e2e-test-kind E2E_TEST_TAGS="autoresize"

# Run specific test
make e2e-test-kind E2E_TEST_TAGS="autoresize" \
    GINKGO_OPTS="--focus='resizes data PVC'"

# Run with WAL safety tests (requires backup config)
BACKUP_DESTINATION_PATH=s3://bucket/path \
BACKUP_ENDPOINT_URL=https://s3.example.com \
make e2e-test-kind E2E_TEST_TAGS="autoresize"

# Run at higher test depth for full coverage
TEST_DEPTH=4 make e2e-test-kind E2E_TEST_TAGS="autoresize"
```

### Test Timeouts

| Test Scenario | Recommended Timeout | Reason |
|--------------|---------------------|--------|
| Basic resize | 300s | CSI expansion can be slow |
| Archive failure detection | 120s | Need WAL accumulation |
| Slot retention | 180s | Need WAL generation and slot lag |
| Cooldown verification | 90s | Must wait for cooldown period |
| Metrics verification | 60s | Quick metric scrape |

---

## CI/CD Considerations

### Storage Class Requirements

Auto-resize tests require a storage class with `allowVolumeExpansion: true`. The tests automatically skip if this isn't available.

For kind clusters:
```yaml
# kind-config.yaml
apiVersion: kind.x-k8s.io/v1alpha4
kind: Cluster
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /tmp/kind-volumes
        containerPath: /var/local-path-provisioner
---
# After cluster creation, patch the default storage class
kubectl patch storageclass standard -p '{"allowVolumeExpansion": true}'
```

### Backup Testing Infrastructure

WAL safety tests require a backup destination. Options:

1. **MinIO in CI** - Deploy MinIO as part of test setup
2. **Mock endpoint** - Use a deliberately failing endpoint
3. **Skip in basic CI** - Only run full tests in nightly builds

### Parallelization

Auto-resize tests can run in parallel with other storage tests but not with each other (resource contention on small test clusters).

```go
// Use Ordered for tests that must run sequentially
Context("WAL safety mechanisms", Ordered, func() {
    // Tests run in order within this context
})
```

### Resource Requirements

| Test Type | Pods | PVCs | Memory | Duration |
|-----------|------|------|--------|----------|
| Basic resize | 3 | 6 | 2Gi | 5-10min |
| WAL safety | 3 | 6 | 2Gi | 10-15min |
| Single volume | 1 | 1 | 512Mi | 5min |
| Tablespace | 3 | 9 | 3Gi | 10min |

---

## Summary

### Test Coverage Matrix

| Feature | Unit Test | Integration Test | E2E Test |
|---------|-----------|------------------|----------|
| Threshold calculation | ✓ | ✓ | ✓ |
| Size increase (% and abs) | ✓ | ✓ | ✓ |
| MaxSize enforcement | ✓ | | ✓ |
| Cooldown enforcement | ✓ | | ✓ |
| WAL archive health check | ✓ | ✓ | ✓ |
| Pending WAL files check | ✓ | ✓ | ✓ |
| Inactive slot detection | ✓ | ✓ | ✓ |
| Single-volume acknowledgment | ✓ | ✓ | ✓ |
| Disk metrics (statfs) | ✓ | ✓ | ✓ |
| Resize event recording | ✓ | | ✓ |
| Prometheus metrics | | ✓ | ✓ |
| Tablespace resize | ✓ | | ✓ |

### Implementation Order

1. **Phase 1** - Basic tests (threshold, resize, metrics)
2. **Phase 2** - Safety tests (maxSize, cooldown)
3. **Phase 3** - WAL safety tests (archive health, slots)
4. **Phase 4** - Tablespace and edge case tests

### Files to Create/Modify

| File | Action |
|------|--------|
| `tests/e2e/pvc_autoresize_test.go` | Create |
| `tests/e2e/asserts_test.go` | Add helper functions |
| `tests/labels.go` | Add `LabelAutoResize` |
| `tests/e2e/fixtures/pvc_autoresize/*.yaml.template` | Create (8 fixtures) |
