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

package e2e

import (
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/specs"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	"github.com/cloudnative-pg/cloudnative-pg/tests"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/clusterutils"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/proxy"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/storage"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("PVC Auto-Resize", Label(tests.LabelAutoResize), func() {
	const (
		level = tests.Medium
	)

	BeforeEach(func() {
		if testLevelEnv.Depth < int(level) {
			Skip("Test depth is lower than the amount requested for this test")
		}
	})

	Context("basic auto-resize with single volume", func() {
		const (
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-basic.yaml.template"
			clusterName = "cluster-autoresize-basic"
		)
		var namespace string

		It("should resize PVC when disk usage exceeds threshold", func(_ SpecContext) {
			const namespacePrefix = "autoresize-basic-e2e"
			var err error

			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("verifying auto-resize is enabled on the cluster", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster.Spec.StorageConfiguration.Resize).ToNot(BeNil())
				Expect(cluster.Spec.StorageConfiguration.Resize.Enabled).To(BeTrue())
			})

			By("filling the disk to trigger auto-resize", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				// Fill the disk to exceed the 80% usage threshold
				// The volume is 2Gi, so writing ~1.7Gi should trigger resize
				commandTimeout := time.Second * 120
				_, _, err = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"sh", "-c",
					"dd if=/dev/zero of=/var/lib/postgresql/data/pgdata/fill_file bs=1M count=1700",
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("waiting for PVC to be resized", func() {
				// The reconciler runs every 30s, give it time to detect and resize
				Eventually(func() bool {
					pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
					if err != nil {
						return false
					}

					for idx := range pvcList.Items {
						pvc := &pvcList.Items[idx]
						// Only check data PVCs for this cluster
						if pvc.Labels[utils.ClusterLabelName] != clusterName {
							continue
						}
						if pvc.Labels[utils.PvcRoleLabelName] != string(utils.PVCRolePgData) {
							continue
						}
						currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
						originalSize := resource.MustParse("2Gi")
						if currentSize.Cmp(originalSize) > 0 {
							return true
						}
					}
					return false
				}, 5*time.Minute, 10*time.Second).Should(BeTrue(),
					"PVC should have been resized beyond its original 2Gi")
			})

			By("cleaning up the fill file", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				commandTimeout := time.Second * 30
				_, _, _ = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"rm", "-f", "/var/lib/postgresql/data/pgdata/fill_file",
				)
			})
		})
	})

	Context("auto-resize with separate WAL volume", func() {
		const (
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-wal-runtime.yaml.template"
			clusterName = "cluster-autoresize-wal-runtime"
		)
		var namespace string

		It("should resize WAL PVC when WAL volume usage exceeds threshold", func(_ SpecContext) {
			const namespacePrefix = "autoresize-wal-e2e"
			var err error

			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("verifying both storage and WAL resize are enabled", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster.Spec.StorageConfiguration.Resize).ToNot(BeNil())
				Expect(cluster.Spec.StorageConfiguration.Resize.Enabled).To(BeTrue())
				Expect(cluster.Spec.WalStorage).ToNot(BeNil())
				Expect(cluster.Spec.WalStorage.Resize).ToNot(BeNil())
				Expect(cluster.Spec.WalStorage.Resize.Enabled).To(BeTrue())
			})

			By("verifying PVCs exist for both data and WAL", func() {
				pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
				Expect(err).ToNot(HaveOccurred())

				var dataCount, walCount int
				for idx := range pvcList.Items {
					pvc := &pvcList.Items[idx]
					if pvc.Labels[utils.ClusterLabelName] != clusterName {
						continue
					}
					switch pvc.Labels[utils.PvcRoleLabelName] {
					case string(utils.PVCRolePgData):
						dataCount++
					case string(utils.PVCRolePgWal):
						walCount++
					}
				}
				Expect(dataCount).To(BeNumerically(">", 0), "should have data PVCs")
				Expect(walCount).To(BeNumerically(">", 0), "should have WAL PVCs")
			})

			By("filling the WAL volume to trigger auto-resize", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				// Fill the WAL volume to exceed the 80% usage threshold
				// The WAL volume is 2Gi, so writing ~1.7Gi should trigger resize
				// WAL mount is at /var/lib/postgresql/wal/pg_wal
				commandTimeout := time.Second * 120
				_, _, err = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"sh", "-c",
					"dd if=/dev/zero of=/var/lib/postgresql/wal/pg_wal/fill_file bs=1M count=1700",
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("waiting for WAL PVC to be resized", func() {
				// The reconciler runs every 30s, give it time to detect and resize
				Eventually(func() bool {
					pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
					if err != nil {
						return false
					}

					for idx := range pvcList.Items {
						pvc := &pvcList.Items[idx]
						// Only check WAL PVCs for this cluster
						if pvc.Labels[utils.ClusterLabelName] != clusterName {
							continue
						}
						if pvc.Labels[utils.PvcRoleLabelName] != string(utils.PVCRolePgWal) {
							continue
						}
						currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
						originalSize := resource.MustParse("2Gi")
						if currentSize.Cmp(originalSize) > 0 {
							return true
						}
					}
					return false
				}, 5*time.Minute, 10*time.Second).Should(BeTrue(),
					"WAL PVC should have been resized beyond its original 2Gi")
			})

			By("cleaning up the fill file", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				commandTimeout := time.Second * 30
				_, _, _ = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"rm", "-f", "/var/lib/postgresql/wal/pg_wal/fill_file",
				)
			})
		})
	})

	Context("auto-resize respects expansion limit", func() {
		const (
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-limit.yaml.template"
			clusterName = "cluster-autoresize-limit"
		)
		var namespace string

		It("should resize PVC but never exceed configured limit", func(_ SpecContext) {
			const namespacePrefix = "autoresize-limit-e2e"
			var err error

			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("verifying expansion limit is set to 3Gi", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster.Spec.StorageConfiguration.Resize.Expansion.Limit).To(Equal("3Gi"))
			})

			By("filling the disk to trigger first auto-resize", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				// Fill the disk to exceed the 80% usage threshold
				// The volume is 2Gi, so writing ~1.7Gi should trigger resize
				commandTimeout := time.Second * 120
				_, _, err = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"sh", "-c",
					"dd if=/dev/zero of=/var/lib/postgresql/data/pgdata/fill_file bs=1M count=1700",
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("waiting for PVC to be resized to 3Gi (the limit)", func() {
				// The reconciler runs every 30s, give it time to detect and resize
				Eventually(func() bool {
					pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
					if err != nil {
						return false
					}

					for idx := range pvcList.Items {
						pvc := &pvcList.Items[idx]
						if pvc.Labels[utils.ClusterLabelName] != clusterName {
							continue
						}
						if pvc.Labels[utils.PvcRoleLabelName] != string(utils.PVCRolePgData) {
							continue
						}
						currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
						limitSize := resource.MustParse("3Gi")
						// Should have grown to the limit
						if currentSize.Cmp(limitSize) >= 0 {
							return true
						}
					}
					return false
				}, 5*time.Minute, 10*time.Second).Should(BeTrue(),
					"PVC should have been resized to the limit of 3Gi")
			})

			By("verifying PVC does not exceed the limit", func() {
				pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
				Expect(err).ToNot(HaveOccurred())

				for idx := range pvcList.Items {
					pvc := &pvcList.Items[idx]
					if pvc.Labels[utils.ClusterLabelName] != clusterName {
						continue
					}
					if pvc.Labels[utils.PvcRoleLabelName] != string(utils.PVCRolePgData) {
						continue
					}
					currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
					limitSize := resource.MustParse("3Gi")
					// Should not exceed limit
					Expect(currentSize.Cmp(limitSize)).To(BeNumerically("<=", 0),
						"PVC size should not exceed limit of 3Gi")
				}
			})

			By("cleaning up the fill file", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				commandTimeout := time.Second * 30
				_, _, _ = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"rm", "-f", "/var/lib/postgresql/data/pgdata/fill_file",
				)
			})
		})
	})

	Context("webhook validation", func() {
		It("should reject auto-resize for single-volume clusters without acknowledgeWALRisk", func(_ SpecContext) {
			const namespacePrefix = "autoresize-webhook-e2e"

			namespace, err := env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			cluster := &apiv1.Cluster{}
			cluster.SetName("autoresize-no-ack")
			cluster.SetNamespace(namespace)
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Size: "2Gi",
				Resize: &apiv1.ResizeConfiguration{
					Enabled: true,
					// No strategy with acknowledgeWALRisk → should be rejected
				},
			}
			cluster.Spec.Bootstrap = &apiv1.BootstrapConfiguration{
				InitDB: &apiv1.BootstrapInitDB{
					Database: "app",
					Owner:    "app",
				},
			}

			err = env.Client.Create(env.Ctx, cluster)
			Expect(err).To(HaveOccurred(),
				"cluster creation should fail without acknowledgeWALRisk for single-volume")
		})

		It("should accept auto-resize for single-volume clusters with acknowledgeWALRisk", func(_ SpecContext) {
			const namespacePrefix = "autoresize-ack-e2e"

			namespace, err := env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			cluster := &apiv1.Cluster{}
			cluster.SetName("autoresize-with-ack")
			cluster.SetNamespace(namespace)
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Size: "2Gi",
				Resize: &apiv1.ResizeConfiguration{
					Enabled: true,
					Strategy: &apiv1.ResizeStrategy{
						WALSafetyPolicy: &apiv1.WALSafetyPolicy{
							AcknowledgeWALRisk: true,
						},
					},
				},
			}
			cluster.Spec.Bootstrap = &apiv1.BootstrapConfiguration{
				InitDB: &apiv1.BootstrapInitDB{
					Database: "app",
					Owner:    "app",
				},
			}

			err = env.Client.Create(env.Ctx, cluster)
			Expect(err).ToNot(HaveOccurred(),
				"cluster creation should succeed with acknowledgeWALRisk for single-volume")
		})
	})

	Context("rate-limit enforcement", func() {
		const (
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-ratelimit.yaml.template"
			clusterName = "cluster-autoresize-ratelimit"
		)
		var namespace string

		It("should block second resize when rate limit exhausted", func(_ SpecContext) {
			const namespacePrefix = "autoresize-ratelimit-e2e"
			var err error

			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("verifying maxActionsPerDay is set to 1", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster.Spec.StorageConfiguration.Resize.Strategy).ToNot(BeNil())
				Expect(cluster.Spec.StorageConfiguration.Resize.Strategy.MaxActionsPerDay).To(Equal(1))
			})

			By("filling the disk to trigger first auto-resize", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				// Fill the disk to exceed the 80% usage threshold
				commandTimeout := time.Second * 120
				_, _, err = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"sh", "-c",
					"dd if=/dev/zero of=/var/lib/postgresql/data/pgdata/fill_file bs=1M count=1700",
				)
				Expect(err).ToNot(HaveOccurred())
			})

			var sizeAfterFirstResize resource.Quantity
			By("waiting for first resize to succeed", func() {
				Eventually(func() bool {
					pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
					if err != nil {
						return false
					}

					for idx := range pvcList.Items {
						pvc := &pvcList.Items[idx]
						if pvc.Labels[utils.ClusterLabelName] != clusterName {
							continue
						}
						if pvc.Labels[utils.PvcRoleLabelName] != string(utils.PVCRolePgData) {
							continue
						}
						currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
						originalSize := resource.MustParse("2Gi")
						if currentSize.Cmp(originalSize) > 0 {
							sizeAfterFirstResize = currentSize
							return true
						}
					}
					return false
				}, 5*time.Minute, 10*time.Second).Should(BeTrue(),
					"First resize should succeed")
			})

			By("attempting to trigger second resize (rate limit should block)", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				// Remove old fill file and create a new one to trigger resize again
				commandTimeout := time.Second * 30
				_, _, _ = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"rm", "-f", "/var/lib/postgresql/data/pgdata/fill_file",
				)

				// Try to fill the disk again - this may fail with "no space left" if rate
				// limiting prevented the resize, which is actually expected behavior.
				// We'll verify the PVC size didn't change in the next step.
				commandTimeout = time.Second * 120
				_, _, _ = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"sh", "-c",
					"dd if=/dev/zero of=/var/lib/postgresql/data/pgdata/fill_file2 bs=1M count=2000 || true",
				)
				// Ignore the error - the important thing is whether resize was blocked
			})

			By("verifying second resize is blocked by rate limit", func() {
				// Wait 2 minutes and verify size hasn't changed
				time.Sleep(2 * time.Minute)

				pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
				Expect(err).ToNot(HaveOccurred())

				for idx := range pvcList.Items {
					pvc := &pvcList.Items[idx]
					if pvc.Labels[utils.ClusterLabelName] != clusterName {
						continue
					}
					if pvc.Labels[utils.PvcRoleLabelName] != string(utils.PVCRolePgData) {
						continue
					}
					currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
					Expect(currentSize.Cmp(sizeAfterFirstResize)).To(Equal(0),
						"PVC size should not have changed due to rate limit")
				}
			})

			By("cleaning up the fill files", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				commandTimeout := time.Second * 30
				_, _, _ = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"rm", "-f", "/var/lib/postgresql/data/pgdata/fill_file",
					"/var/lib/postgresql/data/pgdata/fill_file2",
				)
			})
		})
	})

	Context("minStep clamping", func() {
		const (
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-minstep.yaml.template"
			clusterName = "cluster-autoresize-minstep"
		)
		var namespace string

		It("should resize by at least minStep even when step percentage is smaller", func(_ SpecContext) {
			const namespacePrefix = "autoresize-minstep-e2e"
			var err error

			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("verifying minStep is configured to 1Gi with 5% step", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster.Spec.StorageConfiguration.Resize.Expansion).ToNot(BeNil())
				Expect(cluster.Spec.StorageConfiguration.Resize.Expansion.MinStep).To(Equal("1Gi"))
				// 5% of 2Gi = 102Mi, but minStep clamps to 1Gi
			})

			By("filling the disk to trigger auto-resize", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				// Fill the disk to exceed the 80% usage threshold
				commandTimeout := time.Second * 120
				_, _, err = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"sh", "-c",
					"dd if=/dev/zero of=/var/lib/postgresql/data/pgdata/fill_file bs=1M count=1700",
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("waiting for PVC to be resized by at least minStep (1Gi)", func() {
				Eventually(func() bool {
					pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
					if err != nil {
						return false
					}

					for idx := range pvcList.Items {
						pvc := &pvcList.Items[idx]
						if pvc.Labels[utils.ClusterLabelName] != clusterName {
							continue
						}
						if pvc.Labels[utils.PvcRoleLabelName] != string(utils.PVCRolePgData) {
							continue
						}
						currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
						// Original was 2Gi, minStep is 1Gi, so should be at least 3Gi
						expectedMinSize := resource.MustParse("3Gi")
						if currentSize.Cmp(expectedMinSize) >= 0 {
							return true
						}
					}
					return false
				}, 5*time.Minute, 10*time.Second).Should(BeTrue(),
					"PVC should have grown by at least minStep (1Gi) from 2Gi to at least 3Gi")
			})

			By("cleaning up the fill file", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				commandTimeout := time.Second * 30
				_, _, _ = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"rm", "-f", "/var/lib/postgresql/data/pgdata/fill_file",
				)
			})
		})
	})

	Context("maxStep clamping via webhook", func() {
		It("should accept cluster with valid maxStep configuration", func(_ SpecContext) {
			const namespacePrefix = "autoresize-maxstep-e2e"

			namespace, err := env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			cluster := &apiv1.Cluster{}
			cluster.SetName("autoresize-maxstep")
			cluster.SetNamespace(namespace)
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Size: "100Gi",
				Resize: &apiv1.ResizeConfiguration{
					Enabled: true,
					Expansion: &apiv1.ExpansionPolicy{
						Step:    intstr.IntOrString{Type: intstr.String, StrVal: "50%"},
						MaxStep: "10Gi",
					},
					Strategy: &apiv1.ResizeStrategy{
						WALSafetyPolicy: &apiv1.WALSafetyPolicy{
							AcknowledgeWALRisk: true,
						},
					},
				},
			}
			cluster.Spec.Bootstrap = &apiv1.BootstrapConfiguration{
				InitDB: &apiv1.BootstrapInitDB{
					Database: "app",
					Owner:    "app",
				},
			}

			err = env.Client.Create(env.Ctx, cluster)
			Expect(err).ToNot(HaveOccurred(),
				"cluster creation should succeed with maxStep configured")

			By("verifying maxStep is set correctly", func() {
				created, err := clusterutils.Get(env.Ctx, env.Client, namespace, "autoresize-maxstep")
				Expect(err).ToNot(HaveOccurred())
				Expect(created.Spec.StorageConfiguration.Resize.Expansion.MaxStep).To(Equal("10Gi"))
			})
		})
	})

	Context("metrics exposure", func() {
		const (
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-basic.yaml.template"
			clusterName = "cluster-autoresize-basic"
		)
		var namespace string

		It("should expose disk metrics on the metrics endpoint", func(_ SpecContext) {
			const namespacePrefix = "autoresize-metrics-e2e"
			var err error

			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("verifying disk metrics are exposed", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err = env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				Eventually(func(g Gomega) {
					out, err := proxy.RetrieveMetricsFromInstance(env.Ctx, env.Interface, *pod,
						cluster.IsMetricsTLSEnabled())
					g.Expect(err).ToNot(HaveOccurred(), "while getting pod metrics")
					g.Expect(strings.Contains(out, "cnpg_disk_total_bytes")).To(BeTrue(),
						"should expose cnpg_disk_total_bytes metric")
					g.Expect(strings.Contains(out, "cnpg_disk_used_bytes")).To(BeTrue(),
						"should expose cnpg_disk_used_bytes metric")
					g.Expect(strings.Contains(out, "cnpg_disk_available_bytes")).To(BeTrue(),
						"should expose cnpg_disk_available_bytes metric")
					g.Expect(strings.Contains(out, "cnpg_disk_percent_used")).To(BeTrue(),
						"should expose cnpg_disk_percent_used metric")
				}, 60*time.Second, 5*time.Second).Should(Succeed())
			})
		})
	})

	Context("tablespace resize", func() {
		const (
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-tablespace.yaml.template"
			clusterName = "cluster-autoresize-tablespace"
		)
		var namespace string

		It("should resize tablespace PVC when usage exceeds threshold", func(_ SpecContext) {
			const namespacePrefix = "autoresize-tbs-e2e"
			var err error

			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("verifying tablespace resize is configured", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster.Spec.Tablespaces).To(HaveLen(1))
				Expect(cluster.Spec.Tablespaces[0].Name).To(Equal("tbs1"))
				Expect(cluster.Spec.Tablespaces[0].Storage.Resize).ToNot(BeNil())
				Expect(cluster.Spec.Tablespaces[0].Storage.Resize.Enabled).To(BeTrue())
			})

			By("verifying tablespace PVC exists", func() {
				pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
				Expect(err).ToNot(HaveOccurred())

				var tbsCount int
				for idx := range pvcList.Items {
					pvc := &pvcList.Items[idx]
					if pvc.Labels[utils.ClusterLabelName] != clusterName {
						continue
					}
					if pvc.Labels[utils.PvcRoleLabelName] == string(utils.PVCRolePgTablespace) {
						tbsCount++
					}
				}
				Expect(tbsCount).To(BeNumerically(">", 0), "should have tablespace PVCs")
			})

			By("filling the tablespace volume to trigger auto-resize", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				// Fill the tablespace volume to exceed the 80% usage threshold
				// Tablespaces are mounted at /var/lib/postgresql/tablespaces/<name>
				commandTimeout := time.Second * 120
				_, _, err = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"sh", "-c",
					"dd if=/dev/zero of=/var/lib/postgresql/tablespaces/tbs1/fill_file bs=1M count=1700",
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("waiting for tablespace PVC to be resized", func() {
				Eventually(func() bool {
					pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
					if err != nil {
						return false
					}

					for idx := range pvcList.Items {
						pvc := &pvcList.Items[idx]
						if pvc.Labels[utils.ClusterLabelName] != clusterName {
							continue
						}
						if pvc.Labels[utils.PvcRoleLabelName] != string(utils.PVCRolePgTablespace) {
							continue
						}
						currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
						originalSize := resource.MustParse("2Gi")
						if currentSize.Cmp(originalSize) > 0 {
							return true
						}
					}
					return false
				}, 5*time.Minute, 10*time.Second).Should(BeTrue(),
					"Tablespace PVC should have been resized beyond its original 2Gi")
			})

			By("cleaning up the fill file", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				commandTimeout := time.Second * 30
				_, _, _ = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"rm", "-f", "/var/lib/postgresql/tablespaces/tbs1/fill_file",
				)
			})
		})
	})

	Context("WAL safety policy - archive health blocks resize", func() {
		const (
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-archive-block.yaml.template"
			clusterName = "cluster-autoresize-archive-block"
		)
		var namespace string

		It("should block resize when archive is unhealthy", func(_ SpecContext) {
			const namespacePrefix = "autoresize-archiveblock-e2e"
			var err error

			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("verifying requireArchiveHealthy is enabled", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster.Spec.StorageConfiguration.Resize.Strategy.WALSafetyPolicy).ToNot(BeNil())
				Expect(*cluster.Spec.StorageConfiguration.Resize.Strategy.WALSafetyPolicy.RequireArchiveHealthy).To(BeTrue())
			})

			By("generating WAL to trigger archive failures", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				// Generate some WAL to trigger archive attempts
				commandTimeout := time.Second * 60
				for i := 0; i < 5; i++ {
					_, _, _ = env.EventuallyExecCommand(
						env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
						"psql", "-U", "postgres", "-c", "SELECT pg_switch_wal()",
					)
				}
			})

			By("filling the disk to trigger auto-resize", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				// Fill the disk to exceed the 80% usage threshold
				commandTimeout := time.Second * 120
				_, _, err = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"sh", "-c",
					"dd if=/dev/zero of=/var/lib/postgresql/data/pgdata/fill_file bs=1M count=1700",
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("verifying resize is blocked due to unhealthy archive", func() {
				// Wait 2 minutes and verify PVC has NOT been resized
				time.Sleep(2 * time.Minute)

				pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
				Expect(err).ToNot(HaveOccurred())

				for idx := range pvcList.Items {
					pvc := &pvcList.Items[idx]
					if pvc.Labels[utils.ClusterLabelName] != clusterName {
						continue
					}
					if pvc.Labels[utils.PvcRoleLabelName] != string(utils.PVCRolePgData) {
						continue
					}
					currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
					originalSize := resource.MustParse("2Gi")
					Expect(currentSize.Cmp(originalSize)).To(Equal(0),
						"PVC should NOT have been resized due to unhealthy archive")
				}
			})

			By("cleaning up the fill file", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				commandTimeout := time.Second * 30
				_, _, _ = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"rm", "-f", "/var/lib/postgresql/data/pgdata/fill_file",
				)
			})
		})
	})

	Context("WAL safety policy - inactive slot blocks resize", func() {
		const (
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-slot-block.yaml.template"
			clusterName = "cluster-autoresize-slot-block"
		)
		var namespace string

		It("should block resize when replication slot retains too much WAL", func(_ SpecContext) {
			const namespacePrefix = "autoresize-slotblock-e2e"
			var err error

			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("verifying maxSlotRetentionBytes is configured", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster.Spec.StorageConfiguration.Resize.Strategy.WALSafetyPolicy).ToNot(BeNil())
				// maxSlotRetentionBytes is set to 100MB in the fixture
				Expect(*cluster.Spec.StorageConfiguration.Resize.Strategy.WALSafetyPolicy.MaxSlotRetentionBytes).To(
					Equal(int64(104857600)))
			})

			By("creating an inactive replication slot", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				commandTimeout := time.Second * 30
				_, _, err = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"psql", "-U", "postgres", "-c",
					"SELECT pg_create_physical_replication_slot('test_inactive_slot')",
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("generating WAL to cause slot retention", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				// Generate enough WAL to exceed maxSlotRetentionBytes (100MB)
				// Each pg_switch_wal creates a 16MB WAL segment
				commandTimeout := time.Second * 60
				for i := 0; i < 10; i++ {
					_, _, _ = env.EventuallyExecCommand(
						env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
						"psql", "-U", "postgres", "-c", "SELECT pg_switch_wal()",
					)
					time.Sleep(time.Second)
				}
			})

			By("filling the disk to trigger auto-resize", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				// Fill the disk to exceed the 80% usage threshold
				commandTimeout := time.Second * 120
				_, _, err = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"sh", "-c",
					"dd if=/dev/zero of=/var/lib/postgresql/data/pgdata/fill_file bs=1M count=1700",
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("verifying resize is blocked due to inactive slot retention", func() {
				// Wait 2 minutes and verify PVC has NOT been resized
				time.Sleep(2 * time.Minute)

				pvcList, err := storage.GetPVCList(env.Ctx, env.Client, namespace)
				Expect(err).ToNot(HaveOccurred())

				for idx := range pvcList.Items {
					pvc := &pvcList.Items[idx]
					if pvc.Labels[utils.ClusterLabelName] != clusterName {
						continue
					}
					if pvc.Labels[utils.PvcRoleLabelName] != string(utils.PVCRolePgData) {
						continue
					}
					currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
					originalSize := resource.MustParse("2Gi")
					Expect(currentSize.Cmp(originalSize)).To(Equal(0),
						"PVC should NOT have been resized due to inactive slot retention")
				}
			})

			By("cleaning up", func() {
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				commandTimeout := time.Second * 30
				// Drop the replication slot
				_, _, _ = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"psql", "-U", "postgres", "-c",
					"SELECT pg_drop_replication_slot('test_inactive_slot')",
				)
				// Remove fill file
				_, _, _ = env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"rm", "-f", "/var/lib/postgresql/data/pgdata/fill_file",
				)
			})
		})
	})
})
