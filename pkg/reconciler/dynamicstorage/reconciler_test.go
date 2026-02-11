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
			c := fake.NewClientBuilder().WithScheme(scheme.BuildWithAllKnownScheme()).Build()
			res, err := Reconcile(ctx, c, cluster, nil, status, nil)
			Expect(err).ToNot(HaveOccurred())
			// Should return empty result to allow cluster controller to continue.
			// The cluster controller will requeue periodically when dynamic storage is enabled.
			Expect(res.IsZero()).To(BeTrue())
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
						utils.PvcRoleLabelName: string(utils.PVCRolePgData),
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
			// Set high threshold to avoid emergency
			cluster.Spec.StorageConfiguration.EmergencyGrow = &apiv1.EmergencyGrowConfig{
				CriticalThreshold:   99,
				CriticalMinimumFree: "100Mi",
			}
			// Close maintenance window by setting it to something in the future (using 6 fields)
			cluster.Spec.StorageConfiguration.MaintenanceWindow = &apiv1.MaintenanceWindowConfig{
				Schedule: "0 0 0 31 2 *", // Feb 31st (never)
			}

			c := fake.NewClientBuilder().WithScheme(scheme.BuildWithAllKnownScheme()).Build()
			res, err := Reconcile(ctx, c, cluster, nil, status, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.IsZero()).To(BeTrue())

			Expect(cluster.Status.StorageSizing.Data.State).To(Equal("PendingGrowth"))
		})
	})
})
