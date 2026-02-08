/*
Copyright The CloudNativePG Contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package autoresize

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Auto-Resize Reconciler", func() {
	var r *Reconciler

	BeforeEach(func() {
		r = &Reconciler{}
	})

	Describe("calculateNewSize", func() {
		It("calculates percentage increase correctly", func() {
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
			Expect(err).ToNot(HaveOccurred())

			// 100Gi + 20% = 120Gi
			expected := resource.MustParse("120Gi")
			Expect(newSize.Cmp(expected)).To(Equal(0))
		})

		It("calculates absolute increase correctly", func() {
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
			Expect(err).ToNot(HaveOccurred())

			// 100Gi + 50Gi = 150Gi
			expected := resource.MustParse("150Gi")
			Expect(newSize.Cmp(expected)).To(Equal(0))
		})

		It("respects maxSize limit", func() {
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
			Expect(err).ToNot(HaveOccurred())

			// 100Gi + 50Gi = 150Gi, but capped at 120Gi
			expected := resource.MustParse("120Gi")
			Expect(newSize.Cmp(expected)).To(Equal(0))
		})

		It("uses default increase when not specified", func() {
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
				Enabled: true,
				// Increase not specified, defaults to 20%
			}

			newSize, err := r.calculateNewSize(pvc, config)
			Expect(err).ToNot(HaveOccurred())

			// 100Gi + 20% = 120Gi
			expected := resource.MustParse("120Gi")
			Expect(newSize.Cmp(expected)).To(Equal(0))
		})
	})

	Describe("evaluateWALSafety", func() {
		It("blocks resize when archive is unhealthy", func() {
			health := &apiv1.WALHealthInfo{
				ArchiveHealthy:      false,
				PendingArchiveFiles: 50,
			}

			policy := &apiv1.WALSafetyPolicy{
				RequireArchiveHealthy: ptr.To(true),
			}

			safe, reason := r.evaluateWALSafety(health, policy)
			Expect(safe).To(BeFalse())
			Expect(reason).ToNot(BeEmpty())
			Expect(reason).To(ContainSubstring("unhealthy"))
		})

		It("allows resize when archive is healthy", func() {
			health := &apiv1.WALHealthInfo{
				ArchiveHealthy:      true,
				PendingArchiveFiles: 2,
			}

			policy := &apiv1.WALSafetyPolicy{
				RequireArchiveHealthy: ptr.To(true),
			}

			safe, reason := r.evaluateWALSafety(health, policy)
			Expect(safe).To(BeTrue())
			Expect(reason).To(BeEmpty())
		})

		It("blocks resize when too many WAL files pending", func() {
			health := &apiv1.WALHealthInfo{
				ArchiveHealthy:      true,
				PendingArchiveFiles: 150,
			}

			maxPending := 100
			policy := &apiv1.WALSafetyPolicy{
				RequireArchiveHealthy: ptr.To(false),
				MaxPendingWALFiles:    &maxPending,
			}

			safe, reason := r.evaluateWALSafety(health, policy)
			Expect(safe).To(BeFalse())
			Expect(reason).To(ContainSubstring("Too many pending WAL files"))
		})

		It("allows resize when pending files under limit", func() {
			health := &apiv1.WALHealthInfo{
				ArchiveHealthy:      true,
				PendingArchiveFiles: 50,
			}

			maxPending := 100
			policy := &apiv1.WALSafetyPolicy{
				RequireArchiveHealthy: ptr.To(false),
				MaxPendingWALFiles:    &maxPending,
			}

			safe, reason := r.evaluateWALSafety(health, policy)
			Expect(safe).To(BeTrue())
			Expect(reason).To(BeEmpty())
		})

		It("blocks resize when inactive slots exist", func() {
			health := &apiv1.WALHealthInfo{
				ArchiveHealthy:           true,
				InactiveReplicationSlots: []string{"slot1", "slot2"},
			}

			maxSlotBytes := int64(500 * 1024 * 1024) // 500MB
			policy := &apiv1.WALSafetyPolicy{
				RequireArchiveHealthy: ptr.To(false),
				MaxSlotRetentionBytes: &maxSlotBytes,
			}

			safe, reason := r.evaluateWALSafety(health, policy)
			Expect(safe).To(BeFalse())
			Expect(reason).To(ContainSubstring("Inactive replication slots"))
		})

		It("allows resize when no policy is set", func() {
			health := &apiv1.WALHealthInfo{
				ArchiveHealthy:      false,
				PendingArchiveFiles: 1000,
			}

			safe, reason := r.evaluateWALSafety(health, nil)
			Expect(safe).To(BeTrue())
			Expect(reason).To(BeEmpty())
		})
	})

	Describe("hasAutoResizeEnabled", func() {
		It("returns true when data storage has auto-resize enabled", func() {
			cluster := &apiv1.Cluster{
				Spec: apiv1.ClusterSpec{
					StorageConfiguration: apiv1.StorageConfiguration{
						AutoResize: &apiv1.AutoResizeConfiguration{
							Enabled: true,
						},
					},
				},
			}

			Expect(r.hasAutoResizeEnabled(cluster)).To(BeTrue())
		})

		It("returns true when WAL storage has auto-resize enabled", func() {
			cluster := &apiv1.Cluster{
				Spec: apiv1.ClusterSpec{
					StorageConfiguration: apiv1.StorageConfiguration{},
					WalStorage: &apiv1.StorageConfiguration{
						AutoResize: &apiv1.AutoResizeConfiguration{
							Enabled: true,
						},
					},
				},
			}

			Expect(r.hasAutoResizeEnabled(cluster)).To(BeTrue())
		})

		It("returns true when tablespace has auto-resize enabled", func() {
			cluster := &apiv1.Cluster{
				Spec: apiv1.ClusterSpec{
					StorageConfiguration: apiv1.StorageConfiguration{},
					Tablespaces: []apiv1.TablespaceConfiguration{
						{
							Name: "hot_data",
							Storage: apiv1.StorageConfiguration{
								AutoResize: &apiv1.AutoResizeConfiguration{
									Enabled: true,
								},
							},
						},
					},
				},
			}

			Expect(r.hasAutoResizeEnabled(cluster)).To(BeTrue())
		})

		It("returns false when no auto-resize is enabled", func() {
			cluster := &apiv1.Cluster{
				Spec: apiv1.ClusterSpec{
					StorageConfiguration: apiv1.StorageConfiguration{},
				},
			}

			Expect(r.hasAutoResizeEnabled(cluster)).To(BeFalse())
		})

		It("returns false when auto-resize config exists but is disabled", func() {
			cluster := &apiv1.Cluster{
				Spec: apiv1.ClusterSpec{
					StorageConfiguration: apiv1.StorageConfiguration{
						AutoResize: &apiv1.AutoResizeConfiguration{
							Enabled: false,
						},
					},
				},
			}

			Expect(r.hasAutoResizeEnabled(cluster)).To(BeFalse())
		})
	})

	Describe("cooldownExpired", func() {
		It("returns true when no previous resize recorded", func() {
			cluster := &apiv1.Cluster{
				Status: apiv1.ClusterStatus{},
			}

			Expect(r.cooldownExpired(cluster, "test-pvc", nil)).To(BeTrue())
		})

		It("returns true when last resize was for different PVC", func() {
			cluster := &apiv1.Cluster{
				Status: apiv1.ClusterStatus{
					DiskStatus: &apiv1.ClusterDiskStatus{
						LastAutoResize: &apiv1.AutoResizeEvent{
							PVCName: "other-pvc",
							Time:    metav1.Now(),
						},
					},
				},
			}

			Expect(r.cooldownExpired(cluster, "test-pvc", nil)).To(BeTrue())
		})

		It("returns false when resize was recent", func() {
			cluster := &apiv1.Cluster{
				Status: apiv1.ClusterStatus{
					DiskStatus: &apiv1.ClusterDiskStatus{
						LastAutoResize: &apiv1.AutoResizeEvent{
							PVCName: "test-pvc",
							Time:    metav1.Now(), // Just now
						},
					},
				},
			}

			Expect(r.cooldownExpired(cluster, "test-pvc", nil)).To(BeFalse())
		})
	})

	Describe("findPVC", func() {
		It("finds existing PVC by name", func() {
			pvcs := []corev1.PersistentVolumeClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pvc-2"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pvc-3"}},
			}

			found := r.findPVC(pvcs, "pvc-2")
			Expect(found).ToNot(BeNil())
			Expect(found.Name).To(Equal("pvc-2"))
		})

		It("returns nil when PVC not found", func() {
			pvcs := []corev1.PersistentVolumeClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"}},
			}

			found := r.findPVC(pvcs, "nonexistent")
			Expect(found).To(BeNil())
		})
	})
})
