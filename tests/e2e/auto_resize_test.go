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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/specs"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	"github.com/cloudnative-pg/cloudnative-pg/tests"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/clusterutils"
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
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-wal.yaml.template"
			clusterName = "cluster-autoresize-wal"
		)
		var namespace string

		It("should create cluster with both data and WAL resize enabled", func(_ SpecContext) {
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
		})
	})

	Context("auto-resize respects expansion limit", func() {
		const (
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-basic.yaml.template"
			clusterName = "cluster-autoresize-basic"
		)
		var namespace string

		It("should have the configured expansion limit", func(_ SpecContext) {
			const namespacePrefix = "autoresize-limit-e2e"
			var err error

			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("verifying expansion limit is set", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster.Spec.StorageConfiguration.Resize.Expansion.Limit).To(Equal("10Gi"))
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
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-basic.yaml.template"
			clusterName = "cluster-autoresize-basic"
		)
		var namespace string

		It("should have the configured maxActionsPerDay", func(_ SpecContext) {
			const namespacePrefix = "autoresize-ratelimit-e2e"
			var err error

			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("verifying maxActionsPerDay is set", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster.Spec.StorageConfiguration.Resize.Strategy).ToNot(BeNil())
				Expect(cluster.Spec.StorageConfiguration.Resize.Strategy.MaxActionsPerDay).To(Equal(5))
			})
		})
	})

	Context("minStep clamping", func() {
		const (
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-basic.yaml.template"
			clusterName = "cluster-autoresize-basic"
		)
		var namespace string

		It("should have the configured minStep", func(_ SpecContext) {
			const namespacePrefix = "autoresize-minstep-e2e"
			var err error

			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("verifying minStep is configured", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster.Spec.StorageConfiguration.Resize.Expansion).ToNot(BeNil())
				Expect(cluster.Spec.StorageConfiguration.Resize.Expansion.MinStep).To(Equal("1Gi"))
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
				podName := clusterName + "-1"
				pod := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, types.NamespacedName{
					Namespace: namespace,
					Name:      podName,
				}, pod)
				Expect(err).ToNot(HaveOccurred())

				commandTimeout := time.Second * 30
				stdout, _, err := env.EventuallyExecCommand(
					env.Ctx, *pod, specs.PostgresContainerName, &commandTimeout,
					"sh", "-c",
					"curl -s http://localhost:9187/metrics | grep cnpg_disk",
				)
				Expect(err).ToNot(HaveOccurred())
				Expect(stdout).To(ContainSubstring("cnpg_disk_total_bytes"),
					"should expose cnpg_disk_total_bytes metric")
				Expect(stdout).To(ContainSubstring("cnpg_disk_used_bytes"),
					"should expose cnpg_disk_used_bytes metric")
				Expect(stdout).To(ContainSubstring("cnpg_disk_available_bytes"),
					"should expose cnpg_disk_available_bytes metric")
				Expect(stdout).To(ContainSubstring("cnpg_disk_percent_used"),
					"should expose cnpg_disk_percent_used metric")
			})
		})
	})

	Context("tablespace resize", func() {
		const (
			sampleFile  = fixturesDir + "/auto_resize/cluster-autoresize-tablespace.yaml.template"
			clusterName = "cluster-autoresize-tablespace"
		)
		var namespace string

		It("should create cluster with tablespace resize enabled", func(_ SpecContext) {
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
		})
	})
})
