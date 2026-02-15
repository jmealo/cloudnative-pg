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
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/internal/scheme"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type conflictStatusClient struct {
	client.Client
	statusUpdateConflicts int
}

type conflictStatusWriter struct {
	client.SubResourceWriter
	parent *conflictStatusClient
}

func (c *conflictStatusClient) Status() client.SubResourceWriter {
	return &conflictStatusWriter{
		SubResourceWriter: c.Client.Status(),
		parent:            c,
	}
}

func (w *conflictStatusWriter) Update(
	ctx context.Context,
	obj client.Object,
	opts ...client.SubResourceUpdateOption,
) error {
	if w.parent.statusUpdateConflicts > 0 {
		w.parent.statusUpdateConflicts--
		return apierrs.NewConflict(
			schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "clusters"},
			obj.GetName(),
			errors.New("simulated optimistic lock conflict"),
		)
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

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

		It("trigger scheduled WAL growth when WAL usage exceeds buffer", func() {
			// Disable data dynamic sizing so this test focuses on WAL.
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{Size: "10Gi"}
			cluster.Spec.WalStorage = &apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					CriticalThreshold:   99,
					CriticalMinimumFree: "100Mi",
				},
			}

			status := &postgres.PostgresqlStatusList{
				Items: []postgres.PostgresqlStatus{
					{
						Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-1"}},
						WALDiskStatus: &postgres.DiskStatus{
							TotalBytes:     5 * 1024 * 1024 * 1024,
							UsedBytes:      4600 * 1024 * 1024, // ~4.49Gi used (~90%)
							AvailableBytes: 520 * 1024 * 1024,
							PercentUsed:    89.8,
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

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(cluster, &walPVC).
				WithStatusSubresource(cluster).
				Build()

			res, err := Reconcile(ctx, c, cluster, nil, status, []corev1.PersistentVolumeClaim{walPVC})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())

			var updatedPVC corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-1-wal", Namespace: "default"}, &updatedPVC)
			Expect(err).ToNot(HaveOccurred())
			pvcSize := updatedPVC.Spec.Resources.Requests[corev1.ResourceStorage]
			Expect(pvcSize.Cmp(resource.MustParse("6Gi"))).To(Equal(0))

			var updatedCluster apiv1.Cluster
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster", Namespace: "default"}, &updatedCluster)
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedCluster.Status.StorageSizing).ToNot(BeNil())
			Expect(updatedCluster.Status.StorageSizing.WAL).ToNot(BeNil())
			Expect(updatedCluster.Status.StorageSizing.WAL.LastAction).ToNot(BeNil())
			Expect(updatedCluster.Status.StorageSizing.WAL.LastAction.Kind).To(Equal("ScheduledGrow"))
		})

		It("trigger emergency WAL growth when critical threshold is exceeded", func() {
			// Disable data dynamic sizing so this test focuses on WAL.
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{Size: "10Gi"}
			cluster.Spec.WalStorage = &apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					CriticalThreshold:   70,
					CriticalMinimumFree: "100Mi",
				},
			}

			status := &postgres.PostgresqlStatusList{
				Items: []postgres.PostgresqlStatus{
					{
						Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-1"}},
						WALDiskStatus: &postgres.DiskStatus{
							TotalBytes:     5 * 1024 * 1024 * 1024,
							UsedBytes:      4600 * 1024 * 1024, // ~4.49Gi used (~90%)
							AvailableBytes: 520 * 1024 * 1024,
							PercentUsed:    89.8,
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

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(cluster, &walPVC).
				WithStatusSubresource(cluster).
				Build()

			res, err := Reconcile(ctx, c, cluster, nil, status, []corev1.PersistentVolumeClaim{walPVC})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())

			var updatedPVC corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-1-wal", Namespace: "default"}, &updatedPVC)
			Expect(err).ToNot(HaveOccurred())
			pvcSize := updatedPVC.Spec.Resources.Requests[corev1.ResourceStorage]
			Expect(pvcSize.Cmp(resource.MustParse("5Gi"))).To(BeNumerically(">", 0))

			var updatedCluster apiv1.Cluster
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster", Namespace: "default"}, &updatedCluster)
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedCluster.Status.StorageSizing).ToNot(BeNil())
			Expect(updatedCluster.Status.StorageSizing.WAL).ToNot(BeNil())
			Expect(updatedCluster.Status.StorageSizing.WAL.LastAction).ToNot(BeNil())
			Expect(updatedCluster.Status.StorageSizing.WAL.LastAction.Kind).To(Equal("EmergencyGrow"))
		})

		It("not grow WAL volume when already at limit", func() {
			// Disable data dynamic sizing so this test focuses on WAL.
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{Size: "10Gi"}
			cluster.Spec.WalStorage = &apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "5Gi",
				TargetBuffer: ptr.To(20),
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					CriticalThreshold:   99,
					CriticalMinimumFree: "100Mi",
				},
			}

			status := &postgres.PostgresqlStatusList{
				Items: []postgres.PostgresqlStatus{
					{
						Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-1"}},
						WALDiskStatus: &postgres.DiskStatus{
							TotalBytes:     5 * 1024 * 1024 * 1024,
							UsedBytes:      4600 * 1024 * 1024,
							AvailableBytes: 520 * 1024 * 1024,
							PercentUsed:    89.8,
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

			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(cluster, &walPVC).
				WithStatusSubresource(cluster).
				Build()

			res, err := Reconcile(ctx, c, cluster, nil, status, []corev1.PersistentVolumeClaim{walPVC})
			Expect(err).ToNot(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())

			var updatedPVC corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-1-wal", Namespace: "default"}, &updatedPVC)
			Expect(err).ToNot(HaveOccurred())
			pvcSize := updatedPVC.Spec.Resources.Requests[corev1.ResourceStorage]
			Expect(pvcSize.Cmp(resource.MustParse("5Gi"))).To(Equal(0))
		})
	})

	Describe("minPVCSize", func() {
		It("return zero for empty map", func() {
			result := minPVCSize(nil)
			Expect(result.IsZero()).To(BeTrue())
		})

		It("return the smallest of multiple entries", func() {
			sizes := map[string]string{
				"instance-1": "5Gi",
				"instance-2": "10Gi",
				"instance-3": "7Gi",
			}
			result := minPVCSize(sizes)
			expected := resource.MustParse("5Gi")
			Expect(result.Cmp(expected)).To(Equal(0))
		})

		It("skip invalid entries", func() {
			sizes := map[string]string{
				"instance-1": "5Gi",
				"instance-2": "not-a-size",
			}
			result := minPVCSize(sizes)
			expected := resource.MustParse("5Gi")
			Expect(result.Cmp(expected)).To(Equal(0))
		})
	})

	Describe("findMaxUsage", func() {
		It("select instance with highest usage percentage, not highest absolute bytes", func() {
			// This verifies that a smaller PVC near-full is correctly identified
			// over a larger PVC with more absolute bytes used but lower percentage.
			diskStatusMap := map[string]*DiskInfo{
				"instance-1": {
					// Large disk, high absolute usage (90Gi), but only 90% full
					TotalBytes:     100 * 1024 * 1024 * 1024, // 100Gi
					UsedBytes:      90 * 1024 * 1024 * 1024,  // 90Gi used (90%)
					AvailableBytes: 10 * 1024 * 1024 * 1024,  // 10Gi available
				},
				"instance-2": {
					// Small disk, less absolute usage (9.5Gi), but 95% full
					TotalBytes:     10 * 1024 * 1024 * 1024,          // 10Gi
					UsedBytes:      9*1024*1024*1024 + 500*1024*1024, // 9.5Gi used (95%)
					AvailableBytes: 500 * 1024 * 1024,                // 500Mi available (critical!)
				},
			}

			maxUsed, maxTotal, minAvailable, highestUsageInstance := findMaxUsage(diskStatusMap)

			// instance-2 should be selected: 95% > 90% usage
			Expect(highestUsageInstance).To(Equal("instance-2"))
			Expect(maxUsed).To(Equal(uint64(9*1024*1024*1024 + 500*1024*1024)))
			Expect(maxTotal).To(Equal(uint64(10 * 1024 * 1024 * 1024)))

			// minAvailable should still be from instance-2 (500Mi < 10Gi)
			Expect(minAvailable).To(Equal(uint64(500 * 1024 * 1024)))
		})

		It("track minAvailable independently across all instances", func() {
			// minAvailable should come from the instance with least free space,
			// regardless of which instance has highest usage percentage.
			diskStatusMap := map[string]*DiskInfo{
				"instance-1": {
					TotalBytes:     100 * 1024 * 1024 * 1024, // 100Gi
					UsedBytes:      50 * 1024 * 1024 * 1024,  // 50Gi used (50%)
					AvailableBytes: 50 * 1024 * 1024 * 1024,  // 50Gi available
				},
				"instance-2": {
					TotalBytes:     10 * 1024 * 1024 * 1024,          // 10Gi
					UsedBytes:      9*1024*1024*1024 + 500*1024*1024, // 9.5Gi used (95%)
					AvailableBytes: 500 * 1024 * 1024,                // 500Mi available
				},
			}

			_, _, minAvailable, highestUsageInstance := findMaxUsage(diskStatusMap)

			// instance-2 selected by percentage (95% > 50%)
			Expect(highestUsageInstance).To(Equal("instance-2"))
			// minAvailable from instance-2 (500Mi < 50Gi)
			Expect(minAvailable).To(Equal(uint64(500 * 1024 * 1024)))
		})

		It("return zero minAvailable for empty map", func() {
			diskStatusMap := map[string]*DiskInfo{}
			_, _, minAvailable, _ := findMaxUsage(diskStatusMap)
			Expect(minAvailable).To(Equal(uint64(0)))
		})

		It("return correct values for single instance", func() {
			diskStatusMap := map[string]*DiskInfo{
				"instance-1": {
					TotalBytes:     10 * 1024 * 1024 * 1024,
					UsedBytes:      8 * 1024 * 1024 * 1024,
					AvailableBytes: 2 * 1024 * 1024 * 1024,
				},
			}
			maxUsed, maxTotal, minAvailable, instance := findMaxUsage(diskStatusMap)
			Expect(instance).To(Equal("instance-1"))
			Expect(maxUsed).To(Equal(uint64(8 * 1024 * 1024 * 1024)))
			Expect(maxTotal).To(Equal(uint64(10 * 1024 * 1024 * 1024)))
			Expect(minAvailable).To(Equal(uint64(2 * 1024 * 1024 * 1024)))
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

		It("return partial success count when later PVC patch fails", func() {
			// This test verifies that when patching multiple PVCs and one fails,
			// we correctly report how many succeeded before the failure.
			// This is important for understanding split-brain recovery scenarios.
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
			pvc3 := corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-3",
					Namespace: "default",
					Labels: map[string]string{
						utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
						utils.InstanceNameLabelName: "test-cluster-3",
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

			// Only register pvc1 and pvc3 in the client - pvc2 will fail to patch
			c := fake.NewClientBuilder().
				WithScheme(scheme.BuildWithAllKnownScheme()).
				WithObjects(&pvc1, &pvc3).
				Build()

			result := &ReconcileResult{
				Action:      ActionEmergencyGrow,
				VolumeType:  VolumeTypeData,
				CurrentSize: resource.MustParse("5Gi"),
				TargetSize:  resource.MustParse("10Gi"),
			}

			// Pass all 3 PVCs - pvc1 succeeds, pvc2 fails, pvc3 never attempted
			patchedCount, err := patchPVCsForVolume(ctx, c, []corev1.PersistentVolumeClaim{pvc1, pvc2, pvc3}, result)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("test-cluster-2"))
			// pvc1 was patched before pvc2 failed
			Expect(patchedCount).To(Equal(1))

			// Verify pvc1 WAS patched (this is the split-brain state)
			var updatedPVC1 corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-1", Namespace: "default"}, &updatedPVC1)
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedPVC1.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("10Gi")))

			// Verify pvc3 was NOT patched (never reached due to early return)
			var updatedPVC3 corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-3", Namespace: "default"}, &updatedPVC3)
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedPVC3.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("5Gi")))
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

		It("return error when status cannot be recorded after successful patch", func() {
			// Intentionally omit StorageSizing to force updateStatusAfterAction failure.
			cluster.Status.StorageSizing = nil

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
				Action:       ActionScheduledGrow,
				VolumeType:   VolumeTypeData,
				CurrentSize:  resource.MustParse("5Gi"),
				TargetSize:   resource.MustParse("10Gi"),
				InstanceName: "test-cluster-1",
			}

			err := executeAction(ctx, c, cluster, []corev1.PersistentVolumeClaim{pvc}, result)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("while updating status after storage action"))

			// PVC patch still happened before status write failed.
			var updatedPVC corev1.PersistentVolumeClaim
			err = c.Get(ctx, types.NamespacedName{Name: "test-cluster-1", Namespace: "default"}, &updatedPVC)
			Expect(err).ToNot(HaveOccurred())
			pvcSize := updatedPVC.Spec.Resources.Requests[corev1.ResourceStorage]
			Expect(pvcSize.Cmp(resource.MustParse("10Gi"))).To(Equal(0))
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

		It("use the smallest PVC size when instances diverge after partial patching", func() {
			// instance-1 stayed at 5Gi while instance-2 already reached 10Gi.
			// We must continue reconciling from the smallest size to avoid stalling.
			diskStatus := map[string]*DiskInfo{
				"instance-1": {
					TotalBytes:     5 * 1024 * 1024 * 1024,
					UsedBytes:      4600 * 1024 * 1024, // ~4.49Gi, growth needed
					AvailableBytes: 520 * 1024 * 1024,
					PercentUsed:    89.8,
				},
				"instance-2": {
					TotalBytes:     10 * 1024 * 1024 * 1024,
					UsedBytes:      4 * 1024 * 1024 * 1024,
					AvailableBytes: 6 * 1024 * 1024 * 1024,
					PercentUsed:    40,
				},
			}
			pvcSizes := map[string]string{
				"instance-1": "5Gi",
				"instance-2": "10Gi",
			}

			cluster.Spec.StorageConfiguration.Request = "5Gi"
			cluster.Spec.StorageConfiguration.EmergencyGrow = &apiv1.EmergencyGrowConfig{
				CriticalThreshold:   99,
				CriticalMinimumFree: "100Mi",
			}

			result := evaluateSizing(cluster, &cluster.Spec.StorageConfiguration, VolumeTypeData, "", diskStatus, pvcSizes)
			Expect(result.Action).To(Equal(ActionScheduledGrow))
			Expect(result.CurrentSize.Cmp(resource.MustParse("5Gi"))).To(Equal(0))
			Expect(result.TargetSize.Cmp(resource.MustParse("6Gi"))).To(Equal(0))
		})

		It("use the most utilized instance for growth even when absolute used bytes are lower", func() {
			// instance-1 uses more absolute bytes but has 40% free, so no growth needed for it.
			// instance-2 uses fewer absolute bytes but has only 10% free, so growth is required.
			diskStatus := map[string]*DiskInfo{
				"instance-1": {
					TotalBytes:     200 * 1024 * 1024 * 1024, // 200Gi
					UsedBytes:      120 * 1024 * 1024 * 1024, // 60% used
					AvailableBytes: 80 * 1024 * 1024 * 1024,  // 40% free
					PercentUsed:    60,
				},
				"instance-2": {
					TotalBytes:     50 * 1024 * 1024 * 1024, // 50Gi
					UsedBytes:      45 * 1024 * 1024 * 1024, // 90% used
					AvailableBytes: 5 * 1024 * 1024 * 1024,  // 10% free
					PercentUsed:    90,
				},
			}
			pvcSizes := map[string]string{
				"instance-1": "200Gi",
				"instance-2": "50Gi",
			}

			cluster.Spec.StorageConfiguration.Request = "50Gi"
			cluster.Spec.StorageConfiguration.Limit = "500Gi"
			cluster.Spec.StorageConfiguration.EmergencyGrow = &apiv1.EmergencyGrowConfig{
				CriticalThreshold:   99,
				CriticalMinimumFree: "100Mi",
			}

			result := evaluateSizing(cluster, &cluster.Spec.StorageConfiguration, VolumeTypeData, "", diskStatus, pvcSizes)
			Expect(result.Action).To(Equal(ActionScheduledGrow))
			Expect(result.InstanceName).To(Equal("instance-2"))
		})

		It("allow emergency growth above limit when configured", func() {
			diskStatus := map[string]*DiskInfo{
				"instance-1": {
					TotalBytes:     10 * 1024 * 1024 * 1024,
					UsedBytes:      98 * 1024 * 1024 * 100, // ~9.58Gi used, emergency
					AvailableBytes: 200 * 1024 * 1024,
					PercentUsed:    95.8,
				},
			}
			pvcSizes := map[string]string{"instance-1": "10Gi"}

			cluster.Spec.StorageConfiguration.Request = "5Gi"
			cluster.Spec.StorageConfiguration.Limit = "10Gi"
			cluster.Spec.StorageConfiguration.EmergencyGrow = &apiv1.EmergencyGrowConfig{
				CriticalThreshold:      90,
				CriticalMinimumFree:    "1Gi",
				ExceedLimitOnEmergency: ptr.To(true),
			}

			result := evaluateSizing(cluster, &cluster.Spec.StorageConfiguration, VolumeTypeData, "", diskStatus, pvcSizes)
			Expect(result.Action).To(Equal(ActionEmergencyGrow))
			Expect(result.TargetSize.Cmp(resource.MustParse("10Gi"))).To(BeNumerically(">", 0))
		})
	})

	Describe("helper function coverage", func() {
		Describe("GetEffectiveSizeForNewPVC", func() {
			It("return static size when dynamic sizing is disabled", func() {
				cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{Size: "10Gi"}
				Expect(GetEffectiveSizeForNewPVC(cluster, VolumeTypeData, "")).To(Equal("10Gi"))
			})

			It("return request when dynamic sizing is enabled without effective status", func() {
				cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "100Gi",
				}
				cluster.Status.StorageSizing = nil
				Expect(GetEffectiveSizeForNewPVC(cluster, VolumeTypeData, "")).To(Equal("10Gi"))
			})

			It("return effective size from status when available", func() {
				cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "100Gi",
				}
				cluster.Status.StorageSizing = &apiv1.StorageSizingStatus{
					Data: &apiv1.VolumeSizingStatus{
						EffectiveSize: "25Gi",
					},
				}
				Expect(GetEffectiveSizeForNewPVC(cluster, VolumeTypeData, "")).To(Equal("25Gi"))
			})

			It("return empty string for missing tablespace", func() {
				cluster.Spec.Tablespaces = []apiv1.TablespaceConfiguration{}
				Expect(GetEffectiveSizeForNewPVC(cluster, VolumeTypeTablespace, "missing")).To(Equal(""))
			})
		})

		Describe("collectActualSizes", func() {
			It("prefer status capacity and fallback to spec request", func() {
				pvcs := []corev1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "cluster-1",
							Labels: map[string]string{
								utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
								utils.InstanceNameLabelName: "cluster-1",
							},
						},
						Status: corev1.PersistentVolumeClaimStatus{
							Capacity: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "cluster-2",
							Labels: map[string]string{
								utils.PvcRoleLabelName:      string(utils.PVCRolePgData),
								utils.InstanceNameLabelName: "cluster-2",
							},
						},
						Spec: corev1.PersistentVolumeClaimSpec{
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("8Gi"),
								},
							},
						},
					},
				}

				actual := collectActualSizes(pvcs, VolumeTypeData, "")
				Expect(actual).To(HaveKeyWithValue("cluster-1", "10Gi"))
				Expect(actual).To(HaveKeyWithValue("cluster-2", "8Gi"))
			})

			It("skip PVCs without instance labels", func() {
				pvcs := []corev1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "orphan",
							Labels: map[string]string{
								utils.PvcRoleLabelName: string(utils.PVCRolePgData),
							},
						},
					},
				}
				Expect(collectActualSizes(pvcs, VolumeTypeData, "")).To(BeEmpty())
			})
		})

		Describe("collectDiskStatusForVolume", func() {
			It("return nil for nil status list", func() {
				Expect(collectDiskStatusForVolume(nil, VolumeTypeData, "")).To(BeNil())
			})

			It("collect data disk status by pod name", func() {
				statuses := &postgres.PostgresqlStatusList{
					Items: []postgres.PostgresqlStatus{
						{
							Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cluster-1"}},
							DiskStatus: &postgres.DiskStatus{
								TotalBytes:     10,
								UsedBytes:      5,
								AvailableBytes: 5,
								PercentUsed:    50,
							},
						},
					},
				}

				disk := collectDiskStatusForVolume(statuses, VolumeTypeData, "")
				Expect(disk).To(HaveLen(1))
				Expect(disk).To(HaveKey("cluster-1"))
				Expect(disk["cluster-1"].UsedBytes).To(Equal(uint64(5)))
			})
		})

		Describe("updateStatusAfterAction", func() {
			It("return error when volume status is missing", func() {
				cluster.Status.StorageSizing = nil
				result := &ReconcileResult{
					Action:      ActionEmergencyGrow,
					VolumeType:  VolumeTypeData,
					CurrentSize: resource.MustParse("5Gi"),
					TargetSize:  resource.MustParse("10Gi"),
				}

				err := updateStatusAfterAction(cluster, result)
				Expect(err).To(HaveOccurred())
			})
		})

		Describe("conflict handling on status updates", func() {
			It("requeue on optimistic-lock conflict while setting data waiting status", func() {
				status := &postgres.PostgresqlStatusList{
					Items: []postgres.PostgresqlStatus{
						{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-1"}}},
					},
				}

				baseClient := fake.NewClientBuilder().
					WithScheme(scheme.BuildWithAllKnownScheme()).
					WithObjects(cluster).
					WithStatusSubresource(cluster).
					Build()
				c := &conflictStatusClient{Client: baseClient, statusUpdateConflicts: 1}

				res, err := Reconcile(ctx, c, cluster, nil, status, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(time.Second))
			})

			It("requeue on optimistic-lock conflict after data action status update", func() {
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

				baseClient := fake.NewClientBuilder().
					WithScheme(scheme.BuildWithAllKnownScheme()).
					WithObjects(cluster, &pvc).
					WithStatusSubresource(cluster).
					Build()
				c := &conflictStatusClient{Client: baseClient, statusUpdateConflicts: 1}

				res, err := Reconcile(ctx, c, cluster, nil, status, []corev1.PersistentVolumeClaim{pvc})
				Expect(err).ToNot(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(time.Second))
			})

			It("requeue on optimistic-lock conflict while setting tablespace waiting status", func() {
				cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{Size: "10Gi"}
				cluster.Spec.Tablespaces = []apiv1.TablespaceConfiguration{
					{
						Name: "tbs1",
						Storage: apiv1.StorageConfiguration{
							Request: "5Gi",
							Limit:   "20Gi",
						},
					},
				}

				status := &postgres.PostgresqlStatusList{
					Items: []postgres.PostgresqlStatus{
						{Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-1"}}},
					},
				}

				baseClient := fake.NewClientBuilder().
					WithScheme(scheme.BuildWithAllKnownScheme()).
					WithObjects(cluster).
					WithStatusSubresource(cluster).
					Build()
				c := &conflictStatusClient{Client: baseClient, statusUpdateConflicts: 1}

				res, err := Reconcile(ctx, c, cluster, nil, status, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(time.Second))
			})

			It("requeue on optimistic-lock conflict after tablespace action status update", func() {
				cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{Size: "10Gi"}
				cluster.Spec.Tablespaces = []apiv1.TablespaceConfiguration{
					{
						Name: "tbs1",
						Storage: apiv1.StorageConfiguration{
							Request: "5Gi",
							Limit:   "20Gi",
						},
					},
				}

				status := &postgres.PostgresqlStatusList{
					Items: []postgres.PostgresqlStatus{
						{
							Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster-1"}},
							TablespaceDiskStatus: map[string]*postgres.DiskStatus{
								"tbs1": {
									TotalBytes:     5 * 1024 * 1024 * 1024,
									UsedBytes:      4 * 1024 * 1024 * 1024,
									AvailableBytes: 1 * 1024 * 1024 * 1024,
									PercentUsed:    80,
								},
							},
						},
					},
				}
				pvc := corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-cluster-1-tbs1",
						Namespace: "default",
						Labels: map[string]string{
							utils.PvcRoleLabelName:        string(utils.PVCRolePgTablespace),
							utils.TablespaceNameLabelName: "tbs1",
							utils.InstanceNameLabelName:   "test-cluster-1",
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

				baseClient := fake.NewClientBuilder().
					WithScheme(scheme.BuildWithAllKnownScheme()).
					WithObjects(cluster, &pvc).
					WithStatusSubresource(cluster).
					Build()
				c := &conflictStatusClient{Client: baseClient, statusUpdateConflicts: 1}

				res, err := Reconcile(ctx, c, cluster, nil, status, []corev1.PersistentVolumeClaim{pvc})
				Expect(err).ToNot(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(time.Second))
			})
		})
	})
})
