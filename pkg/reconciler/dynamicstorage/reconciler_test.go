/*
Copyright © contributors to CloudNativePG, established as
CloudNativePG a Series of LF Projects, LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package dynamicstorage

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/internal/scheme"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("reconciler", func() {
	var (
		cluster *apiv1.Cluster
		ctx     context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		cluster = &apiv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-cluster",
				Namespace: "default",
			},
			Spec: apiv1.ClusterSpec{
				StorageConfiguration: apiv1.StorageConfiguration{
					Request:      "10Gi",
					Limit:        "200Gi", // Increased to allow growth
					TargetBuffer: ptr.To(20),
				},
			},
		}
	})

	Describe("Reconcile", func() {
		It("do nothing if dynamic sizing is disabled", func() {
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Size: "10Gi",
			}
			c := fake.NewClientBuilder().WithScheme(scheme.BuildWithAllKnownScheme()).Build()
			res, err := Reconcile(ctx, c, cluster, nil, nil, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())
		})

		It("requeue when instances exist but no disk status available", func() {
			// Simulate instances that exist but haven't reported disk status yet
			status := &postgres.PostgresqlStatusList{
				Items: []postgres.PostgresqlStatus{
					{
						Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-1"}},
						// DiskStatus is nil - instance exists but hasn't reported yet
						DiskStatus: nil,
					},
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(cluster).
				WithStatusSubresource(cluster).
				Build()
			res, err := Reconcile(ctx, c, cluster, nil, status, nil)
			Expect(err).ToNot(HaveOccurred())
			// Should requeue to check again soon when instances exist but don't have disk status yet.
			// This allows the reconciler to retry once disk status becomes available.
			Expect(res.RequeueAfter).To(Equal(30 * time.Second))

			// Verify status was persisted with waiting state
			Expect(cluster.Status.StorageSizing).ToNot(BeNil())
			Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
			Expect(cluster.Status.StorageSizing.Data.State).To(Equal(apiv1.VolumeSizingStateWaitingForDiskStatus))
		})

		It("trigger emergency grow when disk is full", func() {
			// 96% used
			status := &postgres.PostgresqlStatusList{
				Items: []postgres.PostgresqlStatus{
					{
						Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-1"}},
						DiskStatus: &postgres.DiskStatus{
							TotalBytes:     100 * 1024 * 1024 * 1024,
							UsedBytes:      96 * 1024 * 1024 * 1024,
							AvailableBytes: 4 * 1024 * 1024 * 1024,
							PercentUsed:    96,
						},
					},
				},
			}
			pvc := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-1",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-1",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("100Gi"),
						},
					},
				},
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(cluster, &pvc).
				WithStatusSubresource(cluster).
				Build()

			res, err := Reconcile(ctx, c, cluster, nil, status, []corev1.PersistentVolumeClaim{pvc})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())

			// Check that PVC was patched in the fake client
			var updatedPVC corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-1", Namespace: "default"}, &updatedPVC)
			Expect(err).ToNot(HaveOccurred())
			// 100Gi + 25% = 125Gi
			expected := resource.MustParse("125Gi")
			actual := updatedPVC.Spec.Resources.Requests[corev1.ResourceStorage]
			Expect(actual.Cmp(expected)).To(Equal(0))

			// Check that cluster status was updated with LastAction
			var updatedCluster apiv1.Cluster
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster", Namespace: "default"}, &updatedCluster)
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedCluster.Status.StorageSizing).ToNot(BeNil())
			Expect(updatedCluster.Status.StorageSizing.Data).ToNot(BeNil())
			Expect(updatedCluster.Status.StorageSizing.Data.LastAction).ToNot(BeNil())
			Expect(updatedCluster.Status.StorageSizing.Data.LastAction.Kind).To(Equal("EmergencyGrow"))
		})

		It("not trigger false growth when filesystem overhead makes TotalBytes less than PVC capacity", func() {
			// Simulate the real-world scenario: 5Gi PVC with ~3% filesystem overhead
			// statfs reports TotalBytes ≈ 4.84Gi (5074592Ki), but PVC is 5Gi
			// With 80% usage on the filesystem, the target would be ~6Gi
			// But without the fix, currentSize was 4.84Gi instead of 5Gi, causing
			// false growth actions like "growing" from 4.84Gi → 5Gi (a no-op)
			filesystemTotal := uint64(5074592 * 1024)        // ~4.84Gi (what statfs reports for a 5Gi PVC)
			filesystemUsed := uint64(3 * 1024 * 1024 * 1024) // 3Gi used (~59%)
			filesystemAvailable := filesystemTotal - filesystemUsed

			status := &postgres.PostgresqlStatusList{
				Items: []postgres.PostgresqlStatus{
					{
						Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-1"}},
						DiskStatus: &postgres.DiskStatus{
							TotalBytes:     filesystemTotal,
							UsedBytes:      filesystemUsed,
							AvailableBytes: filesystemAvailable,
							PercentUsed:    float64(filesystemUsed) / float64(filesystemTotal) * 100,
						},
					},
				},
			}
			pvc := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-1",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-1",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("5Gi"),
						},
					},
				},
				Status: corev1.PersistentVolumeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("5Gi"),
					},
				},
			}

			// Request=5Gi, Limit=200Gi, TargetBuffer=20%
			// 3Gi used / 4.84Gi total filesystem = ~62% used, 38% free → above 20% buffer → no growth needed
			cluster.Spec.StorageConfiguration.Request = "5Gi"

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(cluster, &pvc).
				WithStatusSubresource(cluster).
				Build()

			res, err := Reconcile(ctx, c, cluster, nil, status, []corev1.PersistentVolumeClaim{pvc})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())

			// Should NOT have triggered any growth action
			// The status should show "Balanced" not "Resizing"
			Expect(cluster.Status.StorageSizing).ToNot(BeNil())
			Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
			Expect(cluster.Status.StorageSizing.Data.State).To(Equal(apiv1.VolumeSizingStateBalanced))
		})

		It("use PVC capacity (not filesystem TotalBytes) as currentSize in growth decision", func() {
			// Scenario: PVC is 5Gi, filesystem reports 4.84Gi, disk is 85% full
			// Target with 20% buffer: usedBytes / 0.8 = ~5.2Gi → rounds up to 6Gi
			// With the fix, currentSize=5Gi (from PVC), target=6Gi → real growth needed
			// Without the fix, currentSize=4.84Gi, target=5Gi → false "growth" 4.84Gi→5Gi
			filesystemTotal := uint64(5074592 * 1024)        // ~4.84Gi
			filesystemUsed := uint64(4 * 1024 * 1024 * 1024) // 4Gi used (~83%)
			filesystemAvailable := filesystemTotal - filesystemUsed

			status := &postgres.PostgresqlStatusList{
				Items: []postgres.PostgresqlStatus{
					{
						Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-1"}},
						DiskStatus: &postgres.DiskStatus{
							TotalBytes:     filesystemTotal,
							UsedBytes:      filesystemUsed,
							AvailableBytes: filesystemAvailable,
							PercentUsed:    float64(filesystemUsed) / float64(filesystemTotal) * 100,
						},
					},
				},
			}
			pvc := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-1",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-1",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("5Gi"),
						},
					},
				},
				Status: corev1.PersistentVolumeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("5Gi"),
					},
				},
			}

			cluster.Spec.StorageConfiguration.Request = "5Gi"
			cluster.Spec.StorageConfiguration.EmergencyGrow = &apiv1.EmergencyGrowConfig{
				CriticalThreshold:   99,
				CriticalMinimumFree: "100Mi",
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(cluster, &pvc).
				WithStatusSubresource(cluster).
				Build()

			res, err := Reconcile(ctx, c, cluster, nil, status, []corev1.PersistentVolumeClaim{pvc})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())

			// Should have triggered growth from 5Gi to 6Gi (target = 4Gi/0.8 = 5Gi, rounded up to 5Gi,
			// but that's not > 5Gi currentSize, so actually the target would be clamped)
			// 4Gi used / 0.8 = 5Gi target. currentSize is 5Gi from PVC. 5Gi <= 5Gi → no growth.
			// This is correct! The PVC is already large enough for the usage level.
			// Growth should NOT be triggered since target (5Gi) <= currentSize (5Gi).
			Expect(cluster.Status.StorageSizing).ToNot(BeNil())
			Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
			Expect(cluster.Status.StorageSizing.Data.State).To(Equal(apiv1.VolumeSizingStateBalanced))

			// PVC should NOT have been patched
			var updatedPVC corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-1", Namespace: "default"}, &updatedPVC)
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedPVC.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("5Gi")))
		})

		It("trigger real growth when disk fills beyond PVC capacity buffer", func() {
			// Scenario: PVC is 5Gi, filesystem reports 4.84Gi, disk is 90% full
			// 4.36Gi used / 0.8 = 5.45Gi → rounds up to 6Gi target
			// currentSize = 5Gi (from PVC), target = 6Gi → real growth from 5Gi to 6Gi
			filesystemTotal := uint64(5074592 * 1024) // ~4.84Gi
			filesystemUsed := uint64(4470000 * 1024)  // ~4.36Gi used (~90% of filesystem)
			filesystemAvailable := filesystemTotal - filesystemUsed

			status := &postgres.PostgresqlStatusList{
				Items: []postgres.PostgresqlStatus{
					{
						Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-1"}},
						DiskStatus: &postgres.DiskStatus{
							TotalBytes:     filesystemTotal,
							UsedBytes:      filesystemUsed,
							AvailableBytes: filesystemAvailable,
							PercentUsed:    float64(filesystemUsed) / float64(filesystemTotal) * 100,
						},
					},
				},
			}
			pvc := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-1",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-1",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("5Gi"),
						},
					},
				},
				Status: corev1.PersistentVolumeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("5Gi"),
					},
				},
			}

			cluster.Spec.StorageConfiguration.Request = "5Gi"
			cluster.Spec.StorageConfiguration.EmergencyGrow = &apiv1.EmergencyGrowConfig{
				CriticalThreshold:   99,
				CriticalMinimumFree: "100Mi",
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(cluster, &pvc).
				WithStatusSubresource(cluster).
				Build()

			res, err := Reconcile(ctx, c, cluster, nil, status, []corev1.PersistentVolumeClaim{pvc})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())

			// PVC should have been grown to 6Gi
			// 4.36Gi used / 0.8 = 5.45Gi → rounds to 6Gi, clamped between 5Gi and 200Gi
			var updatedPVC corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-1", Namespace: "default"}, &updatedPVC)
			Expect(err).ToNot(HaveOccurred())
			expected := resource.MustParse("6Gi")
			actual := updatedPVC.Spec.Resources.Requests[corev1.ResourceStorage]
			Expect(actual.Cmp(expected)).To(Equal(0),
				"PVC should grow from 5Gi to 6Gi, got %s", actual.String())
		})

		It("queue growth if maintenance window is closed", func() {
			// 85% used, target buffer is 20% (so should be 100-20=80%)
			// Total size is 50Gi, Limit is 200Gi.
			status := &postgres.PostgresqlStatusList{
				Items: []postgres.PostgresqlStatus{
					{
						Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-1"}},
						DiskStatus: &postgres.DiskStatus{
							TotalBytes:     50 * 1024 * 1024 * 1024,
							UsedBytes:      45 * 1024 * 1024 * 1024,
							AvailableBytes: 5 * 1024 * 1024 * 1024,
							PercentUsed:    90,
						},
					},
				},
			}
			// PVC with proper labels for size collection
			pvc := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-1",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-1",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("50Gi"),
						},
					},
				},
				Status: corev1.PersistentVolumeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("50Gi"),
					},
				},
			}
			// Set high threshold to avoid emergency
			cluster.Spec.StorageConfiguration.EmergencyGrow = &apiv1.EmergencyGrowConfig{
				CriticalThreshold:   99,
				CriticalMinimumFree: "100Mi",
			}
			// Close maintenance window by setting it to something in the future (using 6 fields)
			cluster.Spec.StorageConfiguration.MaintenanceWindow = &apiv1.MaintenanceWindowConfig{
				Schedule: "0 0 0 31 2 *", // Feb 31st (never)
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(cluster, &pvc).
				WithStatusSubresource(cluster).
				Build()
			res, err := Reconcile(ctx, c, cluster, nil, status, []corev1.PersistentVolumeClaim{pvc})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())

			Expect(cluster.Status.StorageSizing.Data.State).To(Equal(apiv1.VolumeSizingStatePendingGrowth))
		})
	})

	Describe("maxPVCSize", func() {
		It("return zero for empty map", func() {
			result := maxPVCSize(nil)
			Expect(result.IsZero()).To(BeTrue())
		})

		It("return zero for empty non-nil map", func() {
			result := maxPVCSize(map[string]string{})
			Expect(result.IsZero()).To(BeTrue())
		})

		It("return the single entry size", func() {
			sizes := map[string]string{"instance-1": "5Gi"}
			result := maxPVCSize(sizes)
			expected := resource.MustParse("5Gi")
			Expect(result.Cmp(expected)).To(Equal(0))
		})

		It("return the largest of multiple entries", func() {
			sizes := map[string]string{
				"instance-1": "5Gi",
				"instance-2": "10Gi",
				"instance-3": "7Gi",
			}
			result := maxPVCSize(sizes)
			expected := resource.MustParse("10Gi")
			Expect(result.Cmp(expected)).To(Equal(0))
		})

		It("skip invalid entries", func() {
			sizes := map[string]string{
				"instance-1": "5Gi",
				"instance-2": "not-a-size",
			}
			result := maxPVCSize(sizes)
			expected := resource.MustParse("5Gi")
			Expect(result.Cmp(expected)).To(Equal(0))
		})
	})

	Describe("patchPVCsForVolume", func() {
		It("return error when PVC patch fails", func() {
			pvc := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-1",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-1",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("5Gi"),
						},
					},
				},
			}

			// Create a client without the PVC object to simulate patch failure
			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				Build()

			result := &ReconcileResult{
				Action:      ActionEmergencyGrow,
				VolumeType:  VolumeTypeData,
				CurrentSize: resource.MustParse("5Gi"),
				TargetSize:  resource.MustParse("10Gi"),
			}

			patchedCount, err := patchPVCsForVolume(ctx, c, []corev1.PersistentVolumeClaim{pvc}, result)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("error patching PVC"))
			Expect(patchedCount).To(Equal(0))
		})

		It("skip PVCs already at target size", func() {
			pvc := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-1",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-1",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("10Gi"), // Already at target
						},
					},
				},
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(&pvc).
				Build()

			result := &ReconcileResult{
				Action:      ActionScheduledGrow,
				VolumeType:  VolumeTypeData,
				CurrentSize: resource.MustParse("5Gi"),
				TargetSize:  resource.MustParse("10Gi"),
			}

			patchedCount, err := patchPVCsForVolume(ctx, c, []corev1.PersistentVolumeClaim{pvc}, result)
			Expect(err).ToNot(HaveOccurred())
			Expect(patchedCount).To(Equal(0)) // No PVCs needed patching
		})

		//nolint:dupl // test fixtures are similar but testing different scenarios
		It("patch multiple PVCs successfully", func() {
			pvc1 := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-1",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-1",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("5Gi"),
						},
					},
				},
			}
			pvc2 := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-2",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-2",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("5Gi"),
						},
					},
				},
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(&pvc1, &pvc2).
				Build()

			result := &ReconcileResult{
				Action:      ActionEmergencyGrow,
				VolumeType:  VolumeTypeData,
				CurrentSize: resource.MustParse("5Gi"),
				TargetSize:  resource.MustParse("10Gi"),
			}

			patchedCount, err := patchPVCsForVolume(ctx, c, []corev1.PersistentVolumeClaim{pvc1, pvc2}, result)
			Expect(err).ToNot(HaveOccurred())
			Expect(patchedCount).To(Equal(2))

			// Verify both PVCs were patched
			var updatedPVC1 corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-1", Namespace: "default"}, &updatedPVC1)
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedPVC1.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("10Gi")))

			var updatedPVC2 corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-2", Namespace: "default"}, &updatedPVC2)
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedPVC2.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("10Gi")))
		})

		//nolint:dupl // test fixtures are similar but testing different scenarios
		It("only patch PVCs matching the volume type", func() {
			dataPVC := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-1",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-1",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("5Gi"),
						},
					},
				},
			}
			walPVC := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-1-wal",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgWal),
						utils.InstanceNameLabelName: "test-cluster-1",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("2Gi"),
						},
					},
				},
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(&dataPVC, &walPVC).
				Build()

			result := &ReconcileResult{
				Action:      ActionEmergencyGrow,
				VolumeType:  VolumeTypeData, // Only data PVCs
				CurrentSize: resource.MustParse("5Gi"),
				TargetSize:  resource.MustParse("10Gi"),
			}

			patchedCount, err := patchPVCsForVolume(ctx, c, []corev1.PersistentVolumeClaim{dataPVC, walPVC}, result)
			Expect(err).ToNot(HaveOccurred())
			Expect(patchedCount).To(Equal(1)) // Only data PVC

			// Verify data PVC was patched
			var updatedDataPVC corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-1", Namespace: "default"}, &updatedDataPVC)
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedDataPVC.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("10Gi")))

			// Verify WAL PVC was NOT patched
			var updatedWalPVC corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-1-wal", Namespace: "default"}, &updatedWalPVC)
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedWalPVC.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("2Gi")))
		})
	})

	Describe("executeAction", func() {
		It("update status after successful action", func() {
			cluster.Status.StorageSizing = &apiv1.StorageSizingStatus{
				Data: &apiv1.VolumeSizingStatus{},
			}

			pvc := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-1",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-1",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("5Gi"),
						},
					},
				},
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(cluster, &pvc).
				WithStatusSubresource(cluster).
				Build()

			result := &ReconcileResult{
				Action:       ActionEmergencyGrow,
				VolumeType:   VolumeTypeData,
				CurrentSize:  resource.MustParse("5Gi"),
				TargetSize:   resource.MustParse("10Gi"),
				InstanceName: "test-cluster-1",
			}

			err := executeAction(ctx, c, cluster, []corev1.PersistentVolumeClaim{pvc}, result)
			Expect(err).ToNot(HaveOccurred())

			// Verify LastAction was set
			Expect(cluster.Status.StorageSizing.Data.LastAction).ToNot(BeNil())
			Expect(cluster.Status.StorageSizing.Data.LastAction.Kind).To(Equal("EmergencyGrow"))
			Expect(cluster.Status.StorageSizing.Data.LastAction.From).To(Equal("5Gi"))
			Expect(cluster.Status.StorageSizing.Data.LastAction.To).To(Equal("10Gi"))
			Expect(cluster.Status.StorageSizing.Data.LastAction.Result).To(Equal("Success"))
			Expect(cluster.Status.StorageSizing.Data.EffectiveSize).To(Equal("10Gi"))
		})

		It("not update status when no PVCs needed patching", func() {
			cluster.Status.StorageSizing = &apiv1.StorageSizingStatus{
				Data: &apiv1.VolumeSizingStatus{},
			}

			pvc := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-1",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-1",
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("10Gi"), // Already at target
						},
					},
				},
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(cluster, &pvc).
				WithStatusSubresource(cluster).
				Build()

			result := &ReconcileResult{
				Action:      ActionScheduledGrow,
				VolumeType:  VolumeTypeData,
				CurrentSize: resource.MustParse("5Gi"),
				TargetSize:  resource.MustParse("10Gi"),
			}

			err := executeAction(ctx, c, cluster, []corev1.PersistentVolumeClaim{pvc}, result)
			Expect(err).ToNot(HaveOccurred())

			// LastAction should NOT be set since no actual patching happened
			Expect(cluster.Status.StorageSizing.Data.LastAction).To(BeNil())
		})
	})

	Describe("evaluateSizing with filesystem overhead", func() {
		It("use PVC capacity as currentSize rather than filesystem TotalBytes", func() {
			// 5Gi PVC, filesystem reports ~4.84Gi TotalBytes due to metadata overhead
			filesystemTotal := uint64(5074592 * 1024)        // ~4.84Gi
			filesystemUsed := uint64(3 * 1024 * 1024 * 1024) // 3Gi (62% of filesystem)
			filesystemAvailable := filesystemTotal - filesystemUsed

			diskStatus := map[string]*DiskInfo{
				"instance-1": {
					TotalBytes:     filesystemTotal,
					UsedBytes:      filesystemUsed,
					AvailableBytes: filesystemAvailable,
					PercentUsed:    float64(filesystemUsed) / float64(filesystemTotal) * 100,
				},
			}
			pvcSizes := map[string]string{"instance-1": "5Gi"}

			result := evaluateSizing(cluster, &cluster.Spec.StorageConfiguration, VolumeTypeData, "", diskStatus, pvcSizes)

			// CurrentSize should be 5Gi (PVC capacity), NOT 4.84Gi (filesystem TotalBytes)
			expectedCurrent := resource.MustParse("5Gi")
			Expect(result.CurrentSize.Cmp(expectedCurrent)).To(Equal(0),
				"currentSize should be 5Gi (PVC), got %s", result.CurrentSize.String())
		})

		It("fall back to filesystem TotalBytes when no PVC sizes available", func() {
			filesystemTotal := uint64(5 * 1024 * 1024 * 1024) // 5Gi exactly
			filesystemUsed := uint64(3 * 1024 * 1024 * 1024)
			filesystemAvailable := filesystemTotal - filesystemUsed

			diskStatus := map[string]*DiskInfo{
				"instance-1": {
					TotalBytes:     filesystemTotal,
					UsedBytes:      filesystemUsed,
					AvailableBytes: filesystemAvailable,
					PercentUsed:    60,
				},
			}

			// No PVC sizes available (empty map)
			result := evaluateSizing(
				cluster, &cluster.Spec.StorageConfiguration, VolumeTypeData, "", diskStatus, map[string]string{})

			// Should fall back to filesystem TotalBytes
			expectedCurrent := resource.MustParse("5Gi")
			Expect(result.CurrentSize.Cmp(expectedCurrent)).To(Equal(0),
				"currentSize should fall back to filesystem TotalBytes (5Gi), got %s", result.CurrentSize.String())
		})

		It("not trigger growth when PVC capacity equals calculated target", func() {
			// The exact scenario from the T2 bug:
			// PVC is 5Gi, filesystem is ~4.84Gi, usage causes target calculation of 5Gi
			// With PVC-based currentSize: 5Gi target <= 5Gi currentSize → NoOp (correct!)
			// With filesystem-based currentSize: 5Gi target > 4.84Gi currentSize → false growth
			filesystemTotal := uint64(5074592 * 1024)        // ~4.84Gi
			filesystemUsed := uint64(4 * 1024 * 1024 * 1024) // 4Gi (83% of filesystem)
			filesystemAvailable := filesystemTotal - filesystemUsed

			diskStatus := map[string]*DiskInfo{
				"instance-1": {
					TotalBytes:     filesystemTotal,
					UsedBytes:      filesystemUsed,
					AvailableBytes: filesystemAvailable,
					PercentUsed:    float64(filesystemUsed) / float64(filesystemTotal) * 100,
				},
			}
			pvcSizes := map[string]string{"instance-1": "5Gi"}

			cluster.Spec.StorageConfiguration.Request = "5Gi"
			cluster.Spec.StorageConfiguration.EmergencyGrow = &apiv1.EmergencyGrowConfig{
				CriticalThreshold:   99,
				CriticalMinimumFree: "100Mi",
			}

			result := evaluateSizing(cluster, &cluster.Spec.StorageConfiguration, VolumeTypeData, "", diskStatus, pvcSizes)

			// 4Gi / 0.8 = 5Gi target. PVC currentSize = 5Gi. 5Gi <= 5Gi → NoOp
			Expect(result.Action).To(Equal(ActionNoOp),
				"should be NoOp when target (5Gi) <= currentSize (5Gi from PVC), got %s", result.Action)
		})

		It("trigger growth when usage exceeds PVC-based current size buffer", func() {
			// PVC is 5Gi, filesystem reports ~4.84Gi, 90% used
			// used=4.36Gi, target = 4.36/0.8 = 5.45Gi → rounds to 6Gi
			// currentSize = 5Gi (PVC). 6Gi > 5Gi → ScheduledGrow
			filesystemTotal := uint64(5074592 * 1024) // ~4.84Gi
			filesystemUsed := uint64(4470000 * 1024)  // ~4.36Gi (~90% of filesystem)
			filesystemAvailable := filesystemTotal - filesystemUsed

			diskStatus := map[string]*DiskInfo{
				"instance-1": {
					TotalBytes:     filesystemTotal,
					UsedBytes:      filesystemUsed,
					AvailableBytes: filesystemAvailable,
					PercentUsed:    float64(filesystemUsed) / float64(filesystemTotal) * 100,
				},
			}
			pvcSizes := map[string]string{"instance-1": "5Gi"}

			cluster.Spec.StorageConfiguration.Request = "5Gi"
			cluster.Spec.StorageConfiguration.EmergencyGrow = &apiv1.EmergencyGrowConfig{
				CriticalThreshold:   99,
				CriticalMinimumFree: "100Mi",
			}

			result := evaluateSizing(cluster, &cluster.Spec.StorageConfiguration, VolumeTypeData, "", diskStatus, pvcSizes)

			// ~4.36Gi / 0.8 = ~5.45Gi → rounds to 6Gi. 6Gi > 5Gi → growth needed
			Expect(result.Action).To(Equal(ActionScheduledGrow),
				"should trigger ScheduledGrow when target (6Gi) > currentSize (5Gi), got %s with reason: %s",
				result.Action, result.Reason)
			Expect(result.TargetSize.Cmp(resource.MustParse("6Gi"))).To(Equal(0),
				"target should be 6Gi, got %s", result.TargetSize.String())
			Expect(result.CurrentSize.Cmp(resource.MustParse("5Gi"))).To(Equal(0),
				"currentSize should be 5Gi (PVC), got %s", result.CurrentSize.String())
		})
	})
})
