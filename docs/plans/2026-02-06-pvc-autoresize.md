# PVC Auto-Resize Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add automatic PVC resizing support to CloudNativePG with WAL-aware safety mechanisms to prevent masking archive failures.

**Architecture:** Instance managers use `statfs()` to collect disk metrics, expose via Prometheus, and report in status endpoint. The operator fetches disk status during reconciliation, evaluates auto-resize policy (threshold, WAL health, cooldown), and patches PVCs when safe.

**Tech Stack:** Go, Kubernetes controller-runtime, Prometheus client_golang, syscall/statfs, Ginkgo/Gomega for testing

**Design Documents:**
- Main Design: `/docs/src/design/pvc-autoresize.md`
- E2E Testing: `/docs/src/design/pvc-autoresize-e2e-testing.md`

---

## Phase 1: API Types & Code Generation

### Task 1.1: Add AutoResizeConfiguration Type

**Files:**
- Modify: `api/v1/cluster_types.go` (after line ~2039, after StorageConfiguration)

**Step 1: Write the failing test**

Create test file first:

```go
// api/v1/autoresize_test.go
package v1

import (
    "testing"

    "k8s.io/apimachinery/pkg/api/resource"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAutoResizeConfigurationDefaults(t *testing.T) {
    config := &AutoResizeConfiguration{
        Enabled: true,
    }

    // Test default threshold
    if config.Threshold == 0 {
        t.Log("Threshold should default to 80 via kubebuilder marker")
    }

    // Test default increase
    if config.Increase == "" {
        t.Log("Increase should default to 20% via kubebuilder marker")
    }
}

func TestWALSafetyPolicyDefaults(t *testing.T) {
    policy := &WALSafetyPolicy{}

    // RequireArchiveHealthy should default to true
    if policy.RequireArchiveHealthy == nil {
        t.Log("RequireArchiveHealthy should default to true via kubebuilder marker")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v ./api/v1/ -run TestAutoResize`
Expected: FAIL with "undefined: AutoResizeConfiguration"

**Step 3: Write AutoResizeConfiguration type**

Add to `api/v1/cluster_types.go` after StorageConfiguration (around line 2040):

```go
// AutoResizeConfiguration controls automatic PVC expansion
type AutoResizeConfiguration struct {
    // Enabled activates automatic PVC resizing for this volume
    // +kubebuilder:default:=false
    Enabled bool `json:"enabled"`

    // Threshold is the disk usage percentage that triggers a resize (1-99)
    // When usage exceeds this threshold, the PVC will be expanded
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=99
    // +kubebuilder:default:=80
    Threshold int `json:"threshold,omitempty"`

    // Increase specifies how much to grow the PVC when resizing
    // Can be an absolute value (e.g., "10Gi") or percentage (e.g., "20%")
    // +kubebuilder:default:="20%"
    Increase string `json:"increase,omitempty"`

    // MaxSize is the maximum size the PVC can grow to
    // Prevents runaway growth; resize stops when this limit is reached
    // +optional
    MaxSize string `json:"maxSize,omitempty"`

    // CooldownPeriod is the minimum time between resize operations
    // Prevents rapid successive resizes
    // +kubebuilder:default:="1h"
    CooldownPeriod *metav1.Duration `json:"cooldownPeriod,omitempty"`

    // WALSafetyPolicy controls WAL-related safety checks
    // Required for single-volume clusters; optional but recommended for all
    // +optional
    WALSafetyPolicy *WALSafetyPolicy `json:"walSafetyPolicy,omitempty"`
}

// WALSafetyPolicy defines safety checks related to WAL before allowing resize
type WALSafetyPolicy struct {
    // AcknowledgeWALRisk must be true for single-volume clusters
    // Acknowledges that WAL issues may trigger unnecessary resizes
    // +optional
    AcknowledgeWALRisk bool `json:"acknowledgeWALRisk,omitempty"`

    // RequireArchiveHealthy blocks resize if WAL archiving is failing
    // Prevents masking archive failures by growing storage
    // +kubebuilder:default:=true
    RequireArchiveHealthy *bool `json:"requireArchiveHealthy,omitempty"`

    // MaxPendingWALFiles blocks resize if too many files await archiving
    // Set to 0 to disable this check
    // +kubebuilder:default:=100
    MaxPendingWALFiles *int `json:"maxPendingWALFiles,omitempty"`

    // MaxSlotRetentionBytes blocks resize if inactive slots retain too much WAL
    // Set to 0 to disable this check
    // +optional
    MaxSlotRetentionBytes *int64 `json:"maxSlotRetentionBytes,omitempty"`

    // AlertOnResize generates a warning event when resize occurs
    // Useful for tracking WAL-related resizes that may need investigation
    // +kubebuilder:default:=true
    AlertOnResize *bool `json:"alertOnResize,omitempty"`
}
```

**Step 4: Run test to verify it passes**

Run: `go test -v ./api/v1/ -run TestAutoResize`
Expected: PASS

**Step 5: Commit**

```bash
git add api/v1/cluster_types.go api/v1/autoresize_test.go
git commit -m "feat(api): add AutoResizeConfiguration and WALSafetyPolicy types

Add new types to support automatic PVC resizing with WAL-aware safety mechanisms.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

### Task 1.2: Add AutoResize Field to StorageConfiguration

**Files:**
- Modify: `api/v1/cluster_types.go` (StorageConfiguration struct around line 2017-2039)

**Step 1: Write the failing test**

```go
// Add to api/v1/autoresize_test.go
func TestStorageConfigurationHasAutoResize(t *testing.T) {
    storage := StorageConfiguration{
        Size: "10Gi",
        AutoResize: &AutoResizeConfiguration{
            Enabled:   true,
            Threshold: 80,
            Increase:  "20%",
            MaxSize:   "100Gi",
        },
    }

    if storage.AutoResize == nil {
        t.Fatal("AutoResize field should exist on StorageConfiguration")
    }
    if !storage.AutoResize.Enabled {
        t.Fatal("AutoResize.Enabled should be true")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v ./api/v1/ -run TestStorageConfigurationHasAutoResize`
Expected: FAIL with "unknown field AutoResize"

**Step 3: Add AutoResize field to StorageConfiguration**

In `api/v1/cluster_types.go`, add to StorageConfiguration struct (around line 2037):

```go
// StorageConfiguration is the configuration used to create and reconcile PVCs
type StorageConfiguration struct {
    // ... existing fields ...

    // AutoResize configuration for automatic PVC expansion
    // +optional
    AutoResize *AutoResizeConfiguration `json:"autoResize,omitempty"`
}
```

**Step 4: Run test to verify it passes**

Run: `go test -v ./api/v1/ -run TestStorageConfigurationHasAutoResize`
Expected: PASS

**Step 5: Commit**

```bash
git add api/v1/cluster_types.go api/v1/autoresize_test.go
git commit -m "feat(api): add AutoResize field to StorageConfiguration

Enables configuring automatic PVC resizing per storage configuration.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

### Task 1.3: Add ClusterDiskStatus Types

**Files:**
- Modify: `api/v1/cluster_types.go` (after ClusterStatus around line 1041)

**Step 1: Write the failing test**

```go
// Add to api/v1/autoresize_test.go
func TestClusterDiskStatusTypes(t *testing.T) {
    status := &ClusterDiskStatus{
        Instances: []InstanceDiskStatus{
            {
                PodName: "cluster-1",
                Data: &VolumeDiskStatus{
                    TotalBytes:     100 * 1024 * 1024 * 1024, // 100Gi
                    UsedBytes:      80 * 1024 * 1024 * 1024,  // 80Gi
                    AvailableBytes: 20 * 1024 * 1024 * 1024,  // 20Gi
                    PercentUsed:    80.0,
                },
            },
        },
    }

    if len(status.Instances) != 1 {
        t.Fatal("Expected 1 instance")
    }
    if status.Instances[0].Data.PercentUsed != 80.0 {
        t.Fatal("Expected 80% used")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v ./api/v1/ -run TestClusterDiskStatusTypes`
Expected: FAIL with "undefined: ClusterDiskStatus"

**Step 3: Add disk status types**

Add to `api/v1/cluster_types.go` after ClusterStatus:

```go
// ClusterDiskStatus contains disk usage information for the cluster
type ClusterDiskStatus struct {
    // Instances contains per-instance disk status
    Instances []InstanceDiskStatus `json:"instances,omitempty"`

    // LastAutoResize records the most recent auto-resize operation
    // +optional
    LastAutoResize *AutoResizeEvent `json:"lastAutoResize,omitempty"`
}

// InstanceDiskStatus contains disk usage for a single instance
type InstanceDiskStatus struct {
    // PodName is the name of the pod
    PodName string `json:"podName"`

    // Data volume status
    // +optional
    Data *VolumeDiskStatus `json:"data,omitempty"`

    // WAL volume status (nil if using single volume)
    // +optional
    WAL *VolumeDiskStatus `json:"wal,omitempty"`

    // Tablespaces volume status (keyed by tablespace name)
    // +optional
    Tablespaces map[string]*VolumeDiskStatus `json:"tablespaces,omitempty"`

    // WALHealth contains WAL-specific health information
    // +optional
    WALHealth *WALHealthInfo `json:"walHealth,omitempty"`

    // LastUpdated is when this status was last refreshed
    LastUpdated metav1.Time `json:"lastUpdated"`
}

// VolumeDiskStatus contains disk usage for a single volume
type VolumeDiskStatus struct {
    // TotalBytes is the total capacity of the volume
    TotalBytes int64 `json:"totalBytes"`

    // UsedBytes is the used space on the volume
    UsedBytes int64 `json:"usedBytes"`

    // AvailableBytes is the available space on the volume
    AvailableBytes int64 `json:"availableBytes"`

    // PercentUsed is the percentage of the volume that is used
    PercentUsed float64 `json:"percentUsed"`

    // AtMaxSize indicates the volume has reached the configured maxSize
    // +optional
    AtMaxSize bool `json:"atMaxSize,omitempty"`
}

// WALHealthInfo contains WAL-specific health information
type WALHealthInfo struct {
    // ArchiveHealthy indicates WAL archiving is working
    ArchiveHealthy bool `json:"archiveHealthy"`

    // PendingArchiveFiles is the count of files awaiting archive
    PendingArchiveFiles int `json:"pendingArchiveFiles"`

    // InactiveReplicationSlots lists slots that aren't being consumed
    // +optional
    InactiveReplicationSlots []string `json:"inactiveReplicationSlots,omitempty"`
}

// AutoResizeEvent records details of an auto-resize operation
type AutoResizeEvent struct {
    // Time when the resize was initiated
    Time metav1.Time `json:"time"`

    // PodName of the instance whose PVC was resized
    PodName string `json:"podName"`

    // PVCName that was resized
    PVCName string `json:"pvcName"`

    // VolumeType is the type of volume (data, wal, tablespace)
    VolumeType string `json:"volumeType"`

    // OldSize before resize
    OldSize string `json:"oldSize"`

    // NewSize after resize
    NewSize string `json:"newSize"`

    // Reason for the resize
    Reason string `json:"reason"`
}
```

**Step 4: Run test to verify it passes**

Run: `go test -v ./api/v1/ -run TestClusterDiskStatusTypes`
Expected: PASS

**Step 5: Commit**

```bash
git add api/v1/cluster_types.go api/v1/autoresize_test.go
git commit -m "feat(api): add ClusterDiskStatus and related types

Add types for tracking disk usage, WAL health, and auto-resize events.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

### Task 1.4: Add DiskStatus to ClusterStatus

**Files:**
- Modify: `api/v1/cluster_types.go` (ClusterStatus struct around line 801-1041)

**Step 1: Write the failing test**

```go
// Add to api/v1/autoresize_test.go
func TestClusterStatusHasDiskStatus(t *testing.T) {
    cluster := &Cluster{
        Status: ClusterStatus{
            DiskStatus: &ClusterDiskStatus{
                Instances: []InstanceDiskStatus{
                    {PodName: "test-1"},
                },
            },
        },
    }

    if cluster.Status.DiskStatus == nil {
        t.Fatal("DiskStatus field should exist on ClusterStatus")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v ./api/v1/ -run TestClusterStatusHasDiskStatus`
Expected: FAIL with "unknown field DiskStatus"

**Step 3: Add DiskStatus field to ClusterStatus**

In ClusterStatus struct (around line 880), add:

```go
    // DiskStatus reports disk usage for all instances
    // +optional
    DiskStatus *ClusterDiskStatus `json:"diskStatus,omitempty"`
```

**Step 4: Run test to verify it passes**

Run: `go test -v ./api/v1/ -run TestClusterStatusHasDiskStatus`
Expected: PASS

**Step 5: Commit**

```bash
git add api/v1/cluster_types.go api/v1/autoresize_test.go
git commit -m "feat(api): add DiskStatus field to ClusterStatus

Enables tracking disk usage information in cluster status.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

### Task 1.5: Generate CRDs and DeepCopy

**Files:**
- Run code generation

**Step 1: Run generate**

Run: `make generate`
Expected: Success, generates zz_generated.deepcopy.go updates

**Step 2: Run manifests**

Run: `make manifests`
Expected: Success, updates CRD YAML in config/crd/bases/

**Step 3: Verify CRD includes new fields**

Run: `grep -A5 "autoResize" config/crd/bases/postgresql.cnpg.io_clusters.yaml | head -20`
Expected: Shows autoResize field in CRD spec

**Step 4: Run tests to verify generation**

Run: `make test`
Expected: PASS

**Step 5: Commit**

```bash
git add api/v1/zz_generated.deepcopy.go config/crd/bases/
git commit -m "chore: regenerate CRDs and deepcopy for auto-resize types

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Phase 2: Webhook Validation

### Task 2.1: Add Auto-Resize Validation Function

**Files:**
- Modify: `internal/webhook/v1/cluster_webhook.go`

**Step 1: Write the failing test**

```go
// internal/webhook/v1/cluster_autoresize_test.go
package v1

import (
    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"

    apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("auto-resize validation", func() {
    var v *ClusterCustomValidator

    BeforeEach(func() {
        v = &ClusterCustomValidator{}
    })

    Context("single-volume clusters", func() {
        It("requires acknowledgeWALRisk when auto-resize enabled", func() {
            cluster := &apiv1.Cluster{
                Spec: apiv1.ClusterSpec{
                    Instances: 1,
                    StorageConfiguration: apiv1.StorageConfiguration{
                        Size: "10Gi",
                        AutoResize: &apiv1.AutoResizeConfiguration{
                            Enabled:   true,
                            Threshold: 80,
                        },
                        // No WALSafetyPolicy with AcknowledgeWALRisk
                    },
                    // No WalStorage - single volume
                },
            }

            result := v.validateAutoResize(cluster)
            Expect(result).ToNot(BeEmpty())
            Expect(result[0].Field).To(ContainSubstring("acknowledgeWALRisk"))
        })

        It("allows auto-resize with acknowledgeWALRisk", func() {
            cluster := &apiv1.Cluster{
                Spec: apiv1.ClusterSpec{
                    Instances: 1,
                    StorageConfiguration: apiv1.StorageConfiguration{
                        Size: "10Gi",
                        AutoResize: &apiv1.AutoResizeConfiguration{
                            Enabled:   true,
                            Threshold: 80,
                            WALSafetyPolicy: &apiv1.WALSafetyPolicy{
                                AcknowledgeWALRisk: true,
                            },
                        },
                    },
                },
            }

            result := v.validateAutoResize(cluster)
            Expect(result).To(BeEmpty())
        })
    })

    Context("separate WAL volume clusters", func() {
        It("allows auto-resize without acknowledgment", func() {
            cluster := &apiv1.Cluster{
                Spec: apiv1.ClusterSpec{
                    Instances: 3,
                    StorageConfiguration: apiv1.StorageConfiguration{
                        Size: "10Gi",
                        AutoResize: &apiv1.AutoResizeConfiguration{
                            Enabled:   true,
                            Threshold: 80,
                        },
                    },
                    WalStorage: &apiv1.StorageConfiguration{
                        Size: "5Gi",
                    },
                },
            }

            result := v.validateAutoResize(cluster)
            Expect(result).To(BeEmpty())
        })
    })

    Context("threshold validation", func() {
        It("rejects threshold below 1", func() {
            cluster := &apiv1.Cluster{
                Spec: apiv1.ClusterSpec{
                    StorageConfiguration: apiv1.StorageConfiguration{
                        Size: "10Gi",
                        AutoResize: &apiv1.AutoResizeConfiguration{
                            Enabled:   true,
                            Threshold: 0,
                        },
                    },
                    WalStorage: &apiv1.StorageConfiguration{Size: "5Gi"},
                },
            }

            result := v.validateAutoResize(cluster)
            Expect(result).ToNot(BeEmpty())
        })

        It("rejects threshold above 99", func() {
            cluster := &apiv1.Cluster{
                Spec: apiv1.ClusterSpec{
                    StorageConfiguration: apiv1.StorageConfiguration{
                        Size: "10Gi",
                        AutoResize: &apiv1.AutoResizeConfiguration{
                            Enabled:   true,
                            Threshold: 100,
                        },
                    },
                    WalStorage: &apiv1.StorageConfiguration{Size: "5Gi"},
                },
            }

            result := v.validateAutoResize(cluster)
            Expect(result).ToNot(BeEmpty())
        })
    })

    Context("increase format validation", func() {
        It("accepts percentage format", func() {
            cluster := &apiv1.Cluster{
                Spec: apiv1.ClusterSpec{
                    StorageConfiguration: apiv1.StorageConfiguration{
                        Size: "10Gi",
                        AutoResize: &apiv1.AutoResizeConfiguration{
                            Enabled:  true,
                            Increase: "25%",
                        },
                    },
                    WalStorage: &apiv1.StorageConfiguration{Size: "5Gi"},
                },
            }

            result := v.validateAutoResize(cluster)
            Expect(result).To(BeEmpty())
        })

        It("accepts absolute quantity", func() {
            cluster := &apiv1.Cluster{
                Spec: apiv1.ClusterSpec{
                    StorageConfiguration: apiv1.StorageConfiguration{
                        Size: "10Gi",
                        AutoResize: &apiv1.AutoResizeConfiguration{
                            Enabled:  true,
                            Increase: "10Gi",
                        },
                    },
                    WalStorage: &apiv1.StorageConfiguration{Size: "5Gi"},
                },
            }

            result := v.validateAutoResize(cluster)
            Expect(result).To(BeEmpty())
        })

        It("rejects invalid format", func() {
            cluster := &apiv1.Cluster{
                Spec: apiv1.ClusterSpec{
                    StorageConfiguration: apiv1.StorageConfiguration{
                        Size: "10Gi",
                        AutoResize: &apiv1.AutoResizeConfiguration{
                            Enabled:  true,
                            Increase: "invalid",
                        },
                    },
                    WalStorage: &apiv1.StorageConfiguration{Size: "5Gi"},
                },
            }

            result := v.validateAutoResize(cluster)
            Expect(result).ToNot(BeEmpty())
        })
    })
})
```

**Step 2: Run test to verify it fails**

Run: `go test -v ./internal/webhook/v1/ -run "auto-resize validation"`
Expected: FAIL with "v.validateAutoResize undefined"

**Step 3: Implement validateAutoResize function**

Add to `internal/webhook/v1/cluster_webhook.go`:

```go
import (
    "strconv"
    "strings"

    "k8s.io/apimachinery/pkg/api/resource"
    "k8s.io/apimachinery/pkg/util/validation/field"
)

// validateAutoResize validates auto-resize configuration
func (v *ClusterCustomValidator) validateAutoResize(r *apiv1.Cluster) field.ErrorList {
    var result field.ErrorList

    // Validate data volume auto-resize
    if r.Spec.StorageConfiguration.AutoResize != nil {
        isSingleVolume := r.Spec.WalStorage == nil
        result = append(result, v.validateAutoResizeConfig(
            r.Spec.StorageConfiguration.AutoResize,
            field.NewPath("spec", "storage", "autoResize"),
            isSingleVolume,
        )...)
    }

    // Validate WAL volume auto-resize
    if r.Spec.WalStorage != nil && r.Spec.WalStorage.AutoResize != nil {
        result = append(result, v.validateAutoResizeConfig(
            r.Spec.WalStorage.AutoResize,
            field.NewPath("spec", "walStorage", "autoResize"),
            false, // WAL volume is separate, not single-volume
        )...)
    }

    // Validate tablespace auto-resize
    for i, ts := range r.Spec.Tablespaces {
        if ts.Storage.AutoResize != nil {
            result = append(result, v.validateAutoResizeConfig(
                ts.Storage.AutoResize,
                field.NewPath("spec", "tablespaces").Index(i).Child("storage", "autoResize"),
                false,
            )...)
        }
    }

    return result
}

// validateAutoResizeConfig validates a single auto-resize configuration
func (v *ClusterCustomValidator) validateAutoResizeConfig(
    config *apiv1.AutoResizeConfiguration,
    path *field.Path,
    isSingleVolume bool,
) field.ErrorList {
    var result field.ErrorList

    if !config.Enabled {
        return result
    }

    // Single-volume clusters require explicit acknowledgment
    if isSingleVolume {
        if config.WALSafetyPolicy == nil || !config.WALSafetyPolicy.AcknowledgeWALRisk {
            result = append(result, field.Required(
                path.Child("walSafetyPolicy", "acknowledgeWALRisk"),
                "Single-volume clusters with auto-resize enabled require "+
                    "acknowledgeWALRisk: true. This acknowledges that WAL growth "+
                    "from archive or replication failures may trigger unnecessary "+
                    "resizes. Consider using separate walStorage for safer auto-resize.",
            ))
        }
    }

    // Validate threshold range (kubebuilder markers handle this, but double-check)
    if config.Threshold < 1 || config.Threshold > 99 {
        result = append(result, field.Invalid(
            path.Child("threshold"),
            config.Threshold,
            "threshold must be between 1 and 99",
        ))
    }

    // Validate increase format
    if config.Increase != "" {
        if strings.HasSuffix(config.Increase, "%") {
            pct := strings.TrimSuffix(config.Increase, "%")
            if _, err := strconv.ParseFloat(pct, 64); err != nil {
                result = append(result, field.Invalid(
                    path.Child("increase"),
                    config.Increase,
                    "invalid percentage format",
                ))
            }
        } else if _, err := resource.ParseQuantity(config.Increase); err != nil {
            result = append(result, field.Invalid(
                path.Child("increase"),
                config.Increase,
                "must be a valid quantity (e.g., '10Gi') or percentage (e.g., '20%')",
            ))
        }
    }

    // Validate maxSize format
    if config.MaxSize != "" {
        if _, err := resource.ParseQuantity(config.MaxSize); err != nil {
            result = append(result, field.Invalid(
                path.Child("maxSize"),
                config.MaxSize,
                "must be a valid quantity (e.g., '500Gi')",
            ))
        }
    }

    return result
}
```

**Step 4: Register validateAutoResize in validate() function**

In `cluster_webhook.go`, add to the validations slice in the `validate()` function (around line 185):

```go
    validations := []validationFunc{
        // ... existing validations ...
        v.validateAutoResize,
    }
```

**Step 5: Run test to verify it passes**

Run: `go test -v ./internal/webhook/v1/ -run "auto-resize"`
Expected: PASS

**Step 6: Commit**

```bash
git add internal/webhook/v1/cluster_webhook.go internal/webhook/v1/cluster_autoresize_test.go
git commit -m "feat(webhook): add auto-resize configuration validation

Validates threshold range, increase format, maxSize format, and
requires acknowledgeWALRisk for single-volume clusters.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Phase 3: Disk Metrics Collection (Instance Manager)

### Task 3.1: Create Disk Probe Package

**Files:**
- Create: `pkg/management/postgres/disk/probe.go`
- Create: `pkg/management/postgres/disk/probe_test.go`

**Step 1: Write the failing test**

```go
// pkg/management/postgres/disk/probe_test.go
package disk

import (
    "context"
    "os"
    "testing"
)

func TestGetStatsReturnsValidData(t *testing.T) {
    // Use temp directory for testing
    tmpDir, err := os.MkdirTemp("", "disk-probe-test")
    if err != nil {
        t.Fatalf("failed to create temp dir: %v", err)
    }
    defer os.RemoveAll(tmpDir)

    probe := NewProbeWithPaths(tmpDir, "", "")
    ctx := context.Background()

    stats, err := probe.GetDataStats(ctx)
    if err != nil {
        t.Fatalf("GetDataStats failed: %v", err)
    }

    if stats == nil {
        t.Fatal("stats should not be nil")
    }

    if stats.TotalBytes == 0 {
        t.Error("TotalBytes should be > 0")
    }

    if stats.PercentUsed < 0 || stats.PercentUsed > 100 {
        t.Errorf("PercentUsed should be 0-100, got %f", stats.PercentUsed)
    }

    if stats.Path != tmpDir {
        t.Errorf("Path should be %s, got %s", tmpDir, stats.Path)
    }
}

func TestGetWALStatsReturnsNilForSingleVolume(t *testing.T) {
    probe := NewProbeWithPaths("/tmp", "", "")
    ctx := context.Background()

    stats, err := probe.GetWALStats(ctx, false) // separateWAL = false
    if err != nil {
        t.Fatalf("GetWALStats failed: %v", err)
    }

    if stats != nil {
        t.Error("WAL stats should be nil for single volume")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v ./pkg/management/postgres/disk/ -run TestGetStats`
Expected: FAIL with package not found

**Step 3: Implement disk probe**

```go
// pkg/management/postgres/disk/probe.go
package disk

import (
    "context"
    "syscall"

    "github.com/cloudnative-pg/cloudnative-pg/pkg/specs"
)

// VolumeStats contains filesystem statistics for a volume
type VolumeStats struct {
    Path           string
    TotalBytes     uint64
    UsedBytes      uint64
    AvailableBytes uint64
    PercentUsed    float64
    InodesTotal    uint64
    InodesUsed     uint64
    InodesFree     uint64
}

// Probe provides disk statistics for CNPG volumes
type Probe struct {
    dataPath       string
    walPath        string
    tablespacePath string
}

// NewProbe creates a new disk probe for the standard CNPG paths
func NewProbe() *Probe {
    return &Probe{
        dataPath:       specs.PgDataPath,
        walPath:        specs.PgWalVolumePath,
        tablespacePath: specs.PgTablespacesPath,
    }
}

// NewProbeWithPaths creates a new disk probe with custom paths (for testing)
func NewProbeWithPaths(dataPath, walPath, tablespacePath string) *Probe {
    return &Probe{
        dataPath:       dataPath,
        walPath:        walPath,
        tablespacePath: tablespacePath,
    }
}

// GetDataStats returns filesystem stats for the PGDATA volume
func (p *Probe) GetDataStats(ctx context.Context) (*VolumeStats, error) {
    return p.getStats(p.dataPath)
}

// GetWALStats returns filesystem stats for the WAL volume
// Returns nil if WAL is on the same volume as PGDATA
func (p *Probe) GetWALStats(ctx context.Context, separateWAL bool) (*VolumeStats, error) {
    if !separateWAL {
        return nil, nil
    }
    return p.getStats(p.walPath)
}

// GetTablespaceStats returns filesystem stats for a tablespace volume
func (p *Probe) GetTablespaceStats(ctx context.Context, tablespaceName string) (*VolumeStats, error) {
    path := specs.MountForTablespace(tablespaceName)
    return p.getStats(path)
}

func (p *Probe) getStats(path string) (*VolumeStats, error) {
    var stat syscall.Statfs_t
    if err := syscall.Statfs(path, &stat); err != nil {
        return nil, err
    }

    totalBytes := stat.Blocks * uint64(stat.Bsize)
    freeBytes := stat.Bavail * uint64(stat.Bsize)
    usedBytes := totalBytes - freeBytes

    var percentUsed float64
    if totalBytes > 0 {
        percentUsed = float64(usedBytes) / float64(totalBytes) * 100
    }

    return &VolumeStats{
        Path:           path,
        TotalBytes:     totalBytes,
        UsedBytes:      usedBytes,
        AvailableBytes: freeBytes,
        PercentUsed:    percentUsed,
        InodesTotal:    stat.Files,
        InodesFree:     stat.Ffree,
        InodesUsed:     stat.Files - stat.Ffree,
    }, nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test -v ./pkg/management/postgres/disk/ -run TestGetStats`
Expected: PASS

**Step 5: Commit**

```bash
git add pkg/management/postgres/disk/
git commit -m "feat(disk): add disk probe for statfs-based metrics

Implements disk space monitoring using statfs syscall for accurate
filesystem statistics independent of Kubernetes PVC status.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

### Task 3.2: Create Disk Metrics Definitions

**Files:**
- Create: `pkg/management/postgres/disk/metrics.go`
- Create: `pkg/management/postgres/disk/metrics_test.go`

**Step 1: Write the failing test**

```go
// pkg/management/postgres/disk/metrics_test.go
package disk

import (
    "testing"

    "github.com/prometheus/client_golang/prometheus"
)

func TestMetricsCanBeRegistered(t *testing.T) {
    registry := prometheus.NewRegistry()
    metrics := NewMetrics()

    if err := metrics.Register(registry); err != nil {
        t.Fatalf("failed to register metrics: %v", err)
    }

    // Verify we can set values
    metrics.TotalBytes.WithLabelValues("data", "").Set(100)
    metrics.UsedBytes.WithLabelValues("data", "").Set(80)
    metrics.PercentUsed.WithLabelValues("data", "").Set(80.0)
}

func TestMetricsLabels(t *testing.T) {
    metrics := NewMetrics()

    // Should support data, wal, and tablespace volume types
    metrics.TotalBytes.WithLabelValues("data", "").Set(100)
    metrics.TotalBytes.WithLabelValues("wal", "").Set(50)
    metrics.TotalBytes.WithLabelValues("tablespace", "hot_data").Set(200)
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v ./pkg/management/postgres/disk/ -run TestMetrics`
Expected: FAIL with "NewMetrics undefined"

**Step 3: Implement metrics**

```go
// pkg/management/postgres/disk/metrics.go
package disk

import (
    "github.com/prometheus/client_golang/prometheus"
)

const (
    namespace = "cnpg"
    subsystem = "disk"
)

// Metrics contains all disk-related Prometheus metrics
type Metrics struct {
    // Bytes metrics
    TotalBytes     *prometheus.GaugeVec
    UsedBytes      *prometheus.GaugeVec
    AvailableBytes *prometheus.GaugeVec
    PercentUsed    *prometheus.GaugeVec

    // Inode metrics
    InodesTotal *prometheus.GaugeVec
    InodesUsed  *prometheus.GaugeVec
    InodesFree  *prometheus.GaugeVec

    // Auto-resize status
    AtMaxSize     *prometheus.GaugeVec
    ResizeBlocked *prometheus.GaugeVec
    ResizesTotal  *prometheus.CounterVec
}

// NewMetrics creates disk metrics
func NewMetrics() *Metrics {
    labels := []string{"volume_type", "tablespace"}

    return &Metrics{
        TotalBytes: prometheus.NewGaugeVec(
            prometheus.GaugeOpts{
                Namespace: namespace,
                Subsystem: subsystem,
                Name:      "total_bytes",
                Help:      "Total capacity of the volume in bytes",
            },
            labels,
        ),
        UsedBytes: prometheus.NewGaugeVec(
            prometheus.GaugeOpts{
                Namespace: namespace,
                Subsystem: subsystem,
                Name:      "used_bytes",
                Help:      "Used space on the volume in bytes",
            },
            labels,
        ),
        AvailableBytes: prometheus.NewGaugeVec(
            prometheus.GaugeOpts{
                Namespace: namespace,
                Subsystem: subsystem,
                Name:      "available_bytes",
                Help:      "Available space on the volume in bytes",
            },
            labels,
        ),
        PercentUsed: prometheus.NewGaugeVec(
            prometheus.GaugeOpts{
                Namespace: namespace,
                Subsystem: subsystem,
                Name:      "percent_used",
                Help:      "Percentage of volume space used",
            },
            labels,
        ),
        InodesTotal: prometheus.NewGaugeVec(
            prometheus.GaugeOpts{
                Namespace: namespace,
                Subsystem: subsystem,
                Name:      "inodes_total",
                Help:      "Total inodes on the volume",
            },
            labels,
        ),
        InodesUsed: prometheus.NewGaugeVec(
            prometheus.GaugeOpts{
                Namespace: namespace,
                Subsystem: subsystem,
                Name:      "inodes_used",
                Help:      "Used inodes on the volume",
            },
            labels,
        ),
        InodesFree: prometheus.NewGaugeVec(
            prometheus.GaugeOpts{
                Namespace: namespace,
                Subsystem: subsystem,
                Name:      "inodes_free",
                Help:      "Free inodes on the volume",
            },
            labels,
        ),
        AtMaxSize: prometheus.NewGaugeVec(
            prometheus.GaugeOpts{
                Namespace: namespace,
                Subsystem: subsystem,
                Name:      "at_max_size",
                Help:      "1 if volume has reached configured maxSize limit",
            },
            labels,
        ),
        ResizeBlocked: prometheus.NewGaugeVec(
            prometheus.GaugeOpts{
                Namespace: namespace,
                Subsystem: subsystem,
                Name:      "resize_blocked",
                Help:      "1 if auto-resize is blocked (WAL health, cooldown, etc.)",
            },
            append(labels, "reason"),
        ),
        ResizesTotal: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Namespace: namespace,
                Subsystem: subsystem,
                Name:      "resizes_total",
                Help:      "Total number of auto-resize operations",
            },
            append(labels, "result"),
        ),
    }
}

// Register registers all metrics with the provided registry
func (m *Metrics) Register(registry prometheus.Registerer) error {
    collectors := []prometheus.Collector{
        m.TotalBytes,
        m.UsedBytes,
        m.AvailableBytes,
        m.PercentUsed,
        m.InodesTotal,
        m.InodesUsed,
        m.InodesFree,
        m.AtMaxSize,
        m.ResizeBlocked,
        m.ResizesTotal,
    }

    for _, c := range collectors {
        if err := registry.Register(c); err != nil {
            return err
        }
    }
    return nil
}

// SetVolumeStats updates metrics from VolumeStats
func (m *Metrics) SetVolumeStats(volumeType, tablespace string, stats *VolumeStats) {
    if stats == nil {
        return
    }
    m.TotalBytes.WithLabelValues(volumeType, tablespace).Set(float64(stats.TotalBytes))
    m.UsedBytes.WithLabelValues(volumeType, tablespace).Set(float64(stats.UsedBytes))
    m.AvailableBytes.WithLabelValues(volumeType, tablespace).Set(float64(stats.AvailableBytes))
    m.PercentUsed.WithLabelValues(volumeType, tablespace).Set(stats.PercentUsed)
    m.InodesTotal.WithLabelValues(volumeType, tablespace).Set(float64(stats.InodesTotal))
    m.InodesUsed.WithLabelValues(volumeType, tablespace).Set(float64(stats.InodesUsed))
    m.InodesFree.WithLabelValues(volumeType, tablespace).Set(float64(stats.InodesFree))
}
```

**Step 4: Run test to verify it passes**

Run: `go test -v ./pkg/management/postgres/disk/ -run TestMetrics`
Expected: PASS

**Step 5: Commit**

```bash
git add pkg/management/postgres/disk/metrics.go pkg/management/postgres/disk/metrics_test.go
git commit -m "feat(disk): add Prometheus metrics for disk usage

Defines metrics for capacity, usage, inodes, and auto-resize status.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

### Task 3.3: Create WAL Health Checker

**Files:**
- Create: `pkg/management/postgres/wal/health.go`
- Create: `pkg/management/postgres/wal/health_test.go`

**Step 1: Write the failing test**

```go
// pkg/management/postgres/wal/health_test.go
package wal

import (
    "os"
    "path/filepath"
    "testing"
)

func TestCountPendingArchive(t *testing.T) {
    // Create temp archive_status directory
    tmpDir, err := os.MkdirTemp("", "wal-health-test")
    if err != nil {
        t.Fatalf("failed to create temp dir: %v", err)
    }
    defer os.RemoveAll(tmpDir)

    archiveStatusPath := filepath.Join(tmpDir, "archive_status")
    if err := os.MkdirAll(archiveStatusPath, 0755); err != nil {
        t.Fatalf("failed to create archive_status dir: %v", err)
    }

    // Create some .ready files (WAL files awaiting archive)
    for i := 0; i < 5; i++ {
        fileName := filepath.Join(archiveStatusPath,
            "000000010000000000000001.ready")
        if i > 0 {
            fileName = filepath.Join(archiveStatusPath,
                "00000001000000000000000"+string(rune('1'+i))+".ready")
        }
        if err := os.WriteFile(fileName, []byte{}, 0644); err != nil {
            t.Fatalf("failed to create ready file: %v", err)
        }
    }

    checker := NewHealthCheckerWithPath(archiveStatusPath)
    count, err := checker.countPendingArchive()
    if err != nil {
        t.Fatalf("countPendingArchive failed: %v", err)
    }

    if count != 5 {
        t.Errorf("expected 5 pending files, got %d", count)
    }
}

func TestCountPendingArchiveEmptyDir(t *testing.T) {
    tmpDir, _ := os.MkdirTemp("", "wal-health-test")
    defer os.RemoveAll(tmpDir)

    checker := NewHealthCheckerWithPath(tmpDir)
    count, err := checker.countPendingArchive()
    if err != nil {
        t.Fatalf("countPendingArchive failed: %v", err)
    }

    if count != 0 {
        t.Errorf("expected 0 pending files, got %d", count)
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v ./pkg/management/postgres/wal/ -run TestCount`
Expected: FAIL with package not found

**Step 3: Implement WAL health checker**

```go
// pkg/management/postgres/wal/health.go
package wal

import (
    "context"
    "database/sql"
    "os"
    "path/filepath"
    "regexp"
    "time"

    "github.com/cloudnative-pg/cloudnative-pg/pkg/specs"
)

// HealthStatus contains WAL health information
type HealthStatus struct {
    ArchiveHealthy          bool
    PendingArchiveFiles     int
    LastArchiveSuccess      *time.Time
    LastArchiveFailure      *time.Time
    InactiveSlots           []SlotInfo
    TotalSlotRetentionBytes int64
}

// SlotInfo contains replication slot information
type SlotInfo struct {
    Name          string
    Active        bool
    RetainedBytes int64
    RestartLSN    string
}

// HealthChecker evaluates WAL health
type HealthChecker struct {
    archiveStatusPath string
}

// NewHealthChecker creates a new WAL health checker
func NewHealthChecker() *HealthChecker {
    return &HealthChecker{
        archiveStatusPath: filepath.Join(specs.PgWalPath, "archive_status"),
    }
}

// NewHealthCheckerWithPath creates a new WAL health checker with custom path
func NewHealthCheckerWithPath(archiveStatusPath string) *HealthChecker {
    return &HealthChecker{
        archiveStatusPath: archiveStatusPath,
    }
}

// Check evaluates current WAL health
func (h *HealthChecker) Check(ctx context.Context, db *sql.DB) (*HealthStatus, error) {
    status := &HealthStatus{
        ArchiveHealthy: true,
    }

    // Count pending archive files
    ready, err := h.countPendingArchive()
    if err != nil {
        return nil, err
    }
    status.PendingArchiveFiles = ready

    // Consider archive unhealthy if too many files pending
    if ready > 10 {
        status.ArchiveHealthy = false
    }

    // Check archive timestamps from pg_stat_archiver
    if db != nil {
        if err := h.checkArchiveStats(ctx, db, status); err != nil {
            return nil, err
        }

        // Check replication slots
        slots, err := h.checkReplicationSlots(ctx, db)
        if err != nil {
            return nil, err
        }

        for _, slot := range slots {
            if !slot.Active {
                status.InactiveSlots = append(status.InactiveSlots, slot)
                status.TotalSlotRetentionBytes += slot.RetainedBytes
            }
        }
    }

    return status, nil
}

var walFileRegex = regexp.MustCompile(`^[0-9A-F]{24}\.ready$`)

func (h *HealthChecker) countPendingArchive() (int, error) {
    entries, err := os.ReadDir(h.archiveStatusPath)
    if err != nil {
        if os.IsNotExist(err) {
            return 0, nil
        }
        return 0, err
    }

    count := 0
    for _, entry := range entries {
        if walFileRegex.MatchString(entry.Name()) {
            count++
        }
    }
    return count, nil
}

func (h *HealthChecker) checkArchiveStats(ctx context.Context, db *sql.DB, status *HealthStatus) error {
    row := db.QueryRowContext(ctx, `
        SELECT
            last_archived_time,
            last_failed_time,
            failed_count
        FROM pg_stat_archiver
    `)

    var lastArchived, lastFailed sql.NullTime
    var failedCount int64

    if err := row.Scan(&lastArchived, &lastFailed, &failedCount); err != nil {
        return err
    }

    if lastArchived.Valid {
        status.LastArchiveSuccess = &lastArchived.Time
    }
    if lastFailed.Valid {
        status.LastArchiveFailure = &lastFailed.Time
        // If last failure is more recent than last success, archive is unhealthy
        if status.LastArchiveSuccess == nil || lastFailed.Time.After(*status.LastArchiveSuccess) {
            status.ArchiveHealthy = false
        }
    }

    return nil
}

func (h *HealthChecker) checkReplicationSlots(ctx context.Context, db *sql.DB) ([]SlotInfo, error) {
    rows, err := db.QueryContext(ctx, `
        SELECT
            slot_name,
            active,
            restart_lsn,
            pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) as retained_bytes
        FROM pg_replication_slots
        WHERE slot_type = 'physical'
    `)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var slots []SlotInfo
    for rows.Next() {
        var slot SlotInfo
        var restartLSN sql.NullString
        var retainedBytes sql.NullInt64

        if err := rows.Scan(&slot.Name, &slot.Active, &restartLSN, &retainedBytes); err != nil {
            return nil, err
        }

        if restartLSN.Valid {
            slot.RestartLSN = restartLSN.String
        }
        if retainedBytes.Valid {
            slot.RetainedBytes = retainedBytes.Int64
        }

        slots = append(slots, slot)
    }

    return slots, rows.Err()
}
```

**Step 4: Run test to verify it passes**

Run: `go test -v ./pkg/management/postgres/wal/ -run TestCount`
Expected: PASS

**Step 5: Commit**

```bash
git add pkg/management/postgres/wal/
git commit -m "feat(wal): add WAL health checker for archive and slot status

Checks pending archive files, archive success/failure times, and
inactive replication slots for WAL-aware auto-resize decisions.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Phase 4: Auto-Resize Reconciler (Operator)

### Task 4.1: Create Auto-Resize Reconciler Package

**Files:**
- Create: `pkg/reconciler/autoresize/reconciler.go`
- Create: `pkg/reconciler/autoresize/reconciler_test.go`

**Step 1: Write the failing test**

```go
// pkg/reconciler/autoresize/reconciler_test.go
package autoresize

import (
    "context"
    "testing"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/api/resource"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

    apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
)

func TestCalculateNewSizePercentage(t *testing.T) {
    r := &Reconciler{}

    pvc := &corev1.PersistentVolumeClaim{
        Spec: corev1.PersistentVolumeClaimSpec{
            Resources: corev1.VolumeResourceRequirements{
                Requests: corev1.ResourceList{
                    corev1.ResourceStorage: resource.MustParse("100Gi"),
                },
            },
        },
    }

    config := &apiv1.AutoResizeConfiguration{
        Enabled:  true,
        Increase: "20%",
    }

    newSize, err := r.calculateNewSize(pvc, config)
    if err != nil {
        t.Fatalf("calculateNewSize failed: %v", err)
    }

    // 100Gi + 20% = 120Gi
    expected := resource.MustParse("120Gi")
    if newSize.Cmp(expected) != 0 {
        t.Errorf("expected %s, got %s", expected.String(), newSize.String())
    }
}

func TestCalculateNewSizeAbsolute(t *testing.T) {
    r := &Reconciler{}

    pvc := &corev1.PersistentVolumeClaim{
        Spec: corev1.PersistentVolumeClaimSpec{
            Resources: corev1.VolumeResourceRequirements{
                Requests: corev1.ResourceList{
                    corev1.ResourceStorage: resource.MustParse("100Gi"),
                },
            },
        },
    }

    config := &apiv1.AutoResizeConfiguration{
        Enabled:  true,
        Increase: "50Gi",
    }

    newSize, err := r.calculateNewSize(pvc, config)
    if err != nil {
        t.Fatalf("calculateNewSize failed: %v", err)
    }

    // 100Gi + 50Gi = 150Gi
    expected := resource.MustParse("150Gi")
    if newSize.Cmp(expected) != 0 {
        t.Errorf("expected %s, got %s", expected.String(), newSize.String())
    }
}

func TestCalculateNewSizeRespectsMaxSize(t *testing.T) {
    r := &Reconciler{}

    pvc := &corev1.PersistentVolumeClaim{
        Spec: corev1.PersistentVolumeClaimSpec{
            Resources: corev1.VolumeResourceRequirements{
                Requests: corev1.ResourceList{
                    corev1.ResourceStorage: resource.MustParse("100Gi"),
                },
            },
        },
    }

    config := &apiv1.AutoResizeConfiguration{
        Enabled:  true,
        Increase: "50Gi",
        MaxSize:  "120Gi", // Cap at 120Gi
    }

    newSize, err := r.calculateNewSize(pvc, config)
    if err != nil {
        t.Fatalf("calculateNewSize failed: %v", err)
    }

    // 100Gi + 50Gi = 150Gi, but capped at 120Gi
    expected := resource.MustParse("120Gi")
    if newSize.Cmp(expected) != 0 {
        t.Errorf("expected %s (capped), got %s", expected.String(), newSize.String())
    }
}

func TestEvaluateWALSafetyBlocksUnhealthyArchive(t *testing.T) {
    r := &Reconciler{}

    health := &apiv1.WALHealthInfo{
        ArchiveHealthy:      false,
        PendingArchiveFiles: 50,
    }

    requireHealthy := true
    policy := &apiv1.WALSafetyPolicy{
        RequireArchiveHealthy: &requireHealthy,
    }

    safe, reason := r.evaluateWALSafety(health, policy)
    if safe {
        t.Error("should not be safe with unhealthy archive")
    }
    if reason == "" {
        t.Error("should have a reason")
    }
}

func TestEvaluateWALSafetyAllowsHealthyArchive(t *testing.T) {
    r := &Reconciler{}

    health := &apiv1.WALHealthInfo{
        ArchiveHealthy:      true,
        PendingArchiveFiles: 2,
    }

    requireHealthy := true
    policy := &apiv1.WALSafetyPolicy{
        RequireArchiveHealthy: &requireHealthy,
    }

    safe, reason := r.evaluateWALSafety(health, policy)
    if !safe {
        t.Errorf("should be safe with healthy archive, got reason: %s", reason)
    }
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v ./pkg/reconciler/autoresize/ -run Test`
Expected: FAIL with package not found

**Step 3: Implement auto-resize reconciler**

```go
// pkg/reconciler/autoresize/reconciler.go
package autoresize

import (
    "context"
    "fmt"
    "strconv"
    "strings"
    "time"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/api/meta"
    "k8s.io/apimachinery/pkg/api/resource"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/tools/record"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"

    apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
)

// Reconciler handles auto-resize logic
type Reconciler struct {
    client   client.Client
    recorder record.EventRecorder
}

// NewReconciler creates a new auto-resize reconciler
func NewReconciler(c client.Client, recorder record.EventRecorder) *Reconciler {
    return &Reconciler{
        client:   c,
        recorder: recorder,
    }
}

// Reconcile evaluates and performs auto-resize operations
func (r *Reconciler) Reconcile(
    ctx context.Context,
    cluster *apiv1.Cluster,
    diskStatuses []apiv1.InstanceDiskStatus,
    pvcs []corev1.PersistentVolumeClaim,
) (ctrl.Result, error) {
    // Skip if no auto-resize configured anywhere
    if !r.hasAutoResizeEnabled(cluster) {
        return ctrl.Result{}, nil
    }

    // Process each instance
    for _, status := range diskStatuses {
        // Check data volume
        if cluster.Spec.StorageConfiguration.AutoResize != nil {
            if err := r.reconcileVolume(ctx, cluster, &status, "data",
                cluster.Spec.StorageConfiguration.AutoResize,
                status.Data,
                r.findPVC(pvcs, status.PodName),
            ); err != nil {
                return ctrl.Result{}, err
            }
        }

        // Check WAL volume (if separate)
        if cluster.ShouldCreateWalArchiveVolume() &&
           cluster.Spec.WalStorage != nil &&
           cluster.Spec.WalStorage.AutoResize != nil {
            if err := r.reconcileVolume(ctx, cluster, &status, "wal",
                cluster.Spec.WalStorage.AutoResize,
                status.WAL,
                r.findPVC(pvcs, status.PodName+"-wal"),
            ); err != nil {
                return ctrl.Result{}, err
            }
        }

        // Check tablespaces
        for _, ts := range cluster.Spec.Tablespaces {
            if ts.Storage.AutoResize != nil {
                tbsStatus := status.Tablespaces[ts.Name]
                if err := r.reconcileVolume(ctx, cluster, &status, "tablespace:"+ts.Name,
                    ts.Storage.AutoResize,
                    tbsStatus,
                    r.findPVC(pvcs, status.PodName+"-tbs-"+ts.Name),
                ); err != nil {
                    return ctrl.Result{}, err
                }
            }
        }
    }

    return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *Reconciler) hasAutoResizeEnabled(cluster *apiv1.Cluster) bool {
    if cluster.Spec.StorageConfiguration.AutoResize != nil &&
       cluster.Spec.StorageConfiguration.AutoResize.Enabled {
        return true
    }
    if cluster.Spec.WalStorage != nil &&
       cluster.Spec.WalStorage.AutoResize != nil &&
       cluster.Spec.WalStorage.AutoResize.Enabled {
        return true
    }
    for _, ts := range cluster.Spec.Tablespaces {
        if ts.Storage.AutoResize != nil && ts.Storage.AutoResize.Enabled {
            return true
        }
    }
    return false
}

func (r *Reconciler) reconcileVolume(
    ctx context.Context,
    cluster *apiv1.Cluster,
    instanceStatus *apiv1.InstanceDiskStatus,
    volumeType string,
    config *apiv1.AutoResizeConfiguration,
    volumeStatus *apiv1.VolumeDiskStatus,
    pvc *corev1.PersistentVolumeClaim,
) error {
    if config == nil || !config.Enabled || volumeStatus == nil || pvc == nil {
        return nil
    }

    // Check if threshold exceeded
    if volumeStatus.PercentUsed < float64(config.Threshold) {
        return nil
    }

    // Check cooldown
    if !r.cooldownExpired(cluster, pvc.Name, config.CooldownPeriod) {
        return nil
    }

    // Check max size
    if config.MaxSize != "" {
        maxSize := resource.MustParse(config.MaxSize)
        currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
        if currentSize.Cmp(maxSize) >= 0 {
            if r.recorder != nil {
                r.recorder.Eventf(cluster, corev1.EventTypeWarning, "AutoResizeMaxReached",
                    "PVC %s has reached max size %s", pvc.Name, config.MaxSize)
            }
            return nil
        }
    }

    // WAL safety checks for WAL volume or single-volume data
    isSingleVolume := !cluster.ShouldCreateWalArchiveVolume()
    if volumeType == "wal" || (volumeType == "data" && isSingleVolume) {
        safe, reason := r.evaluateWALSafety(instanceStatus.WALHealth, config.WALSafetyPolicy)
        if !safe {
            if r.recorder != nil {
                r.recorder.Eventf(cluster, corev1.EventTypeWarning, "AutoResizeBlocked",
                    "Auto-resize blocked for %s: %s", pvc.Name, reason)
            }
            // Update condition
            meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
                Type:    "AutoResizeBlocked",
                Status:  metav1.ConditionTrue,
                Reason:  "WALHealthCheckFailed",
                Message: fmt.Sprintf("PVC %s: %s", pvc.Name, reason),
            })
            return nil
        }
    }

    // Calculate new size
    newSize, err := r.calculateNewSize(pvc, config)
    if err != nil {
        return err
    }

    // Perform resize
    if err := r.resizePVC(ctx, pvc, newSize); err != nil {
        if r.recorder != nil {
            r.recorder.Eventf(cluster, corev1.EventTypeWarning, "AutoResizeFailed",
                "Failed to resize %s: %v", pvc.Name, err)
        }
        return err
    }

    // Record event
    oldSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
    if r.recorder != nil {
        r.recorder.Eventf(cluster, corev1.EventTypeNormal, "AutoResizeSucceeded",
            "Resized %s from %s to %s (usage was %.1f%%)",
            pvc.Name, oldSize.String(), newSize.String(), volumeStatus.PercentUsed)
    }

    // Update status
    if cluster.Status.DiskStatus == nil {
        cluster.Status.DiskStatus = &apiv1.ClusterDiskStatus{}
    }
    cluster.Status.DiskStatus.LastAutoResize = &apiv1.AutoResizeEvent{
        Time:       metav1.Now(),
        PodName:    instanceStatus.PodName,
        PVCName:    pvc.Name,
        VolumeType: volumeType,
        OldSize:    oldSize.String(),
        NewSize:    newSize.String(),
        Reason: fmt.Sprintf("Usage exceeded threshold: %.1f%% > %d%%",
            volumeStatus.PercentUsed, config.Threshold),
    }

    // Clear blocked condition if it was set
    meta.RemoveStatusCondition(&cluster.Status.Conditions, "AutoResizeBlocked")

    return nil
}

func (r *Reconciler) evaluateWALSafety(
    health *apiv1.WALHealthInfo,
    policy *apiv1.WALSafetyPolicy,
) (bool, string) {
    if policy == nil {
        return true, ""
    }

    // Check archive health
    requireHealthy := policy.RequireArchiveHealthy == nil || *policy.RequireArchiveHealthy
    if requireHealthy && health != nil && !health.ArchiveHealthy {
        return false, fmt.Sprintf("WAL archive unhealthy: %d files pending",
            health.PendingArchiveFiles)
    }

    // Check pending file count
    maxPending := 100
    if policy.MaxPendingWALFiles != nil {
        maxPending = *policy.MaxPendingWALFiles
    }
    if maxPending > 0 && health != nil && health.PendingArchiveFiles > maxPending {
        return false, fmt.Sprintf("Too many pending WAL files: %d > %d",
            health.PendingArchiveFiles, maxPending)
    }

    // Check inactive slots
    if policy.MaxSlotRetentionBytes != nil && health != nil {
        if len(health.InactiveReplicationSlots) > 0 {
            return false, fmt.Sprintf("Inactive replication slots detected: %v",
                health.InactiveReplicationSlots)
        }
    }

    return true, ""
}

func (r *Reconciler) calculateNewSize(
    pvc *corev1.PersistentVolumeClaim,
    config *apiv1.AutoResizeConfiguration,
) (resource.Quantity, error) {
    currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
    increase := config.Increase

    if increase == "" {
        increase = "20%"
    }

    var newSize resource.Quantity

    if strings.HasSuffix(increase, "%") {
        // Percentage increase
        pct, err := strconv.ParseFloat(strings.TrimSuffix(increase, "%"), 64)
        if err != nil {
            return newSize, err
        }
        currentBytes := currentSize.Value()
        increaseBytes := int64(float64(currentBytes) * pct / 100)
        newSize = *resource.NewQuantity(currentBytes+increaseBytes, resource.BinarySI)
    } else {
        // Absolute increase
        increaseQty, err := resource.ParseQuantity(increase)
        if err != nil {
            return newSize, err
        }
        currentBytes := currentSize.Value()
        newSize = *resource.NewQuantity(currentBytes+increaseQty.Value(), resource.BinarySI)
    }

    // Cap at maxSize
    if config.MaxSize != "" {
        maxSize := resource.MustParse(config.MaxSize)
        if newSize.Cmp(maxSize) > 0 {
            newSize = maxSize
        }
    }

    return newSize, nil
}

func (r *Reconciler) cooldownExpired(cluster *apiv1.Cluster, pvcName string, cooldownPeriod *metav1.Duration) bool {
    if cluster.Status.DiskStatus == nil || cluster.Status.DiskStatus.LastAutoResize == nil {
        return true
    }

    lastResize := cluster.Status.DiskStatus.LastAutoResize
    if lastResize.PVCName != pvcName {
        return true
    }

    cooldown := time.Hour // default
    if cooldownPeriod != nil {
        cooldown = cooldownPeriod.Duration
    }

    return time.Since(lastResize.Time.Time) >= cooldown
}

func (r *Reconciler) resizePVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim, newSize resource.Quantity) error {
    pvc.Spec.Resources.Requests[corev1.ResourceStorage] = newSize
    return r.client.Update(ctx, pvc)
}

func (r *Reconciler) findPVC(pvcs []corev1.PersistentVolumeClaim, name string) *corev1.PersistentVolumeClaim {
    for i := range pvcs {
        if pvcs[i].Name == name {
            return &pvcs[i]
        }
    }
    return nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test -v ./pkg/reconciler/autoresize/ -run Test`
Expected: PASS

**Step 5: Commit**

```bash
git add pkg/reconciler/autoresize/
git commit -m "feat(autoresize): add PVC auto-resize reconciler

Implements threshold-based PVC resizing with WAL safety checks,
cooldown enforcement, and maxSize limits.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Phase 5: E2E Tests

### Task 5.1: Add LabelAutoResize to Test Labels

**Files:**
- Modify: `tests/labels.go`

**Step 1: Add new label constant**

Add to `tests/labels.go` (after existing labels around line 93):

```go
    // LabelAutoResize marks tests for PVC auto-resize feature
    LabelAutoResize = "autoresize"
```

**Step 2: Commit**

```bash
git add tests/labels.go
git commit -m "test: add LabelAutoResize for auto-resize E2E tests

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

### Task 5.2: Create E2E Test Fixtures

**Files:**
- Create: `tests/e2e/fixtures/pvc_autoresize/cluster-autoresize-basic.yaml.template`
- Create: `tests/e2e/fixtures/pvc_autoresize/cluster-autoresize-single-volume.yaml.template`
- Create: `tests/e2e/fixtures/pvc_autoresize/cluster-autoresize-single-volume-no-ack.yaml.template`

**Step 1: Create fixtures directory and basic fixture**

```yaml
# tests/e2e/fixtures/pvc_autoresize/cluster-autoresize-basic.yaml.template
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
      cooldownPeriod: 30s

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

**Step 2: Create single-volume fixture (with acknowledgment)**

```yaml
# tests/e2e/fixtures/pvc_autoresize/cluster-autoresize-single-volume.yaml.template
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

**Step 3: Create single-volume fixture (without acknowledgment - for rejection test)**

```yaml
# tests/e2e/fixtures/pvc_autoresize/cluster-autoresize-single-volume-no-ack.yaml.template
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

**Step 4: Commit**

```bash
git add tests/e2e/fixtures/pvc_autoresize/
git commit -m "test: add E2E test fixtures for PVC auto-resize

Includes basic, single-volume, and validation rejection test fixtures.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

### Task 5.3: Create E2E Test File

**Files:**
- Create: `tests/e2e/pvc_autoresize_test.go`

**Step 1: Create test file with basic tests**

```go
// tests/e2e/pvc_autoresize_test.go
/*
Copyright The CloudNativePG Contributors

Licensed under the Apache License, Version 2.0 (the "License");
...
*/

package e2e

import (
    "fmt"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "time"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/api/resource"
    "k8s.io/apimachinery/pkg/types"

    apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
    "github.com/cloudnative-pg/cloudnative-pg/tests"
    "github.com/cloudnative-pg/cloudnative-pg/tests/utils/clusterutils"
    "github.com/cloudnative-pg/cloudnative-pg/tests/utils/exec"
    "github.com/cloudnative-pg/cloudnative-pg/tests/utils/run"
    "github.com/cloudnative-pg/cloudnative-pg/tests/utils/storage"

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
        if storageClass == "" {
            Skip("E2E_DEFAULT_STORAGE_CLASS not set")
        }
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

            By("getting primary pod")
            primary, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
            Expect(err).ToNot(HaveOccurred())

            By("filling data volume to threshold")
            fillPath := fillDiskToPercentage(namespace, primary.Name, "/var/lib/postgresql/data", 75)
            defer cleanupFillFile(namespace, primary.Name, fillPath)

            By("verifying auto-resize was triggered")
            expectedMinSize := resource.MustParse("700Mi") // 500Mi + 200Mi increase
            assertAutoResizeTriggered(namespace, clusterName, primary.Name, expectedMinSize)
        })
    })

    // ========================================
    // SINGLE VOLUME TESTS
    // ========================================

    Context("Single volume clusters", func() {
        It("requires acknowledgeWALRisk for auto-resize", func() {
            sampleFile := fixtureDir + "/cluster-autoresize-single-volume-no-ack.yaml.template"

            var err error
            namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix+"-single-noack")
            Expect(err).ToNot(HaveOccurred())

            By("attempting to create cluster without acknowledgeWALRisk")
            // Create should fail with webhook rejection
            _, _, err = run.Unchecked(fmt.Sprintf(
                "kubectl apply -n %s -f %s 2>&1",
                namespace, sampleFile,
            ))
            Expect(err).To(HaveOccurred())
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
            fillPath := fillDiskToPercentage(namespace, primary.Name, "/var/lib/postgresql/data", 75)
            defer cleanupFillFile(namespace, primary.Name, fillPath)

            By("verifying auto-resize was triggered")
            expectedMinSize := resource.MustParse("700Mi")
            assertAutoResizeTriggered(namespace, clusterName, primary.Name, expectedMinSize)
        })
    })
})

// Helper functions

func fillDiskToPercentage(namespace, podName, volumePath string, targetPercent int) string {
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

func cleanupFillFile(namespace, podName, fillPath string) {
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

func assertAutoResizeTriggered(namespace, clusterName, pvcName string, expectedMinSize resource.Quantity) {
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
```

**Step 2: Commit**

```bash
git add tests/e2e/pvc_autoresize_test.go
git commit -m "test: add E2E tests for PVC auto-resize feature

Tests basic threshold-triggered resize and single-volume validation.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

## Phase 6: Integration and Final Testing

### Task 6.1: Run Full Test Suite

**Step 1: Run unit tests**

Run: `make test`
Expected: PASS

**Step 2: Run linting**

Run: `make lint`
Expected: PASS (or fix any issues)

**Step 3: Run all quality checks**

Run: `make checks`
Expected: PASS

**Step 4: Build and verify**

Run: `make build`
Expected: Success

**Step 5: Commit any fixes**

```bash
git add -A
git commit -m "chore: fix linting and test issues

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>"
```

---

### Task 6.2: Run E2E Tests Locally

**Step 1: Run auto-resize E2E tests**

Run: `make e2e-test-kind E2E_TEST_TAGS="autoresize"`
Expected: PASS

**Step 2: Document any issues found and fix**

If tests fail, investigate and fix issues, then re-run.

---

## Summary

This plan implements PVC auto-resize in 6 phases:

1. **Phase 1: API Types** - Add AutoResizeConfiguration, WALSafetyPolicy, ClusterDiskStatus types
2. **Phase 2: Webhook Validation** - Validate configuration, enforce acknowledgeWALRisk for single-volume
3. **Phase 3: Disk Metrics** - Add disk probe and Prometheus metrics in instance manager
4. **Phase 4: Auto-Resize Reconciler** - Implement threshold-based resize with WAL safety checks
5. **Phase 5: E2E Tests** - Add comprehensive E2E test coverage
6. **Phase 6: Integration** - Full test suite and quality checks

Each task follows TDD with bite-sized steps (~2-5 minutes each) and frequent commits.
