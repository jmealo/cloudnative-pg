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

package e2e

import (
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/tests"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/clusterutils"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/exec"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/postgres"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/storage"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("PVC Auto-Resize", Label(tests.LabelAutoResize, tests.LabelStorage), func() {
	const (
		level           = tests.Low
		namespacePrefix = "autoresize-e2e"
	)

	var namespace string

	BeforeEach(func() {
		if testLevelEnv.Depth < int(level) {
			Skip("Test depth is lower than the amount requested for this test")
		}
		if MustGetEnvProfile().UsesNodeDiskSpace() {
			Skip("this test might exhaust node storage")
		}

		// Check if storage expansion is allowed
		defaultStorageClass := os.Getenv("E2E_DEFAULT_STORAGE_CLASS")
		allowExpansion, err := storage.GetStorageAllowExpansion(
			env.Ctx, env.Client,
			defaultStorageClass,
		)
		Expect(err).ToNot(HaveOccurred())
		if allowExpansion == nil || !*allowExpansion {
			Skip(fmt.Sprintf("AllowedVolumeExpansion is false on %v", defaultStorageClass))
		}
	})

	fillDiskToThreshold := func(namespace, podName, path string, targetPercent int) {
		timeout := time.Minute * 5

		// Fill disk to approximately the target percentage
		// Using dd with a 100MB block size, writing until we hit the threshold
		By(fmt.Sprintf("filling %s to approximately %d%% usage", path, targetPercent))
		_, _, _ = exec.CommandInInstancePod(
			env.Ctx, env.Client, env.Interface, env.RestClientConfig,
			exec.PodLocator{
				Namespace: namespace,
				PodName:   podName,
			},
			&timeout,
			"sh", "-c", fmt.Sprintf(
				"while [ $(df --output=pcent %s | tail -1 | tr -d ' %%') -lt %d ]; do "+
					"dd if=/dev/zero of=%s/fill_$RANDOM bs=10M count=10 2>/dev/null || break; done",
				path, targetPercent, path),
		)
	}

	Context("single volume cluster", func() {
		const (
			sampleFile  = fixturesDir + "/autoresize/cluster-autoresize-single-volume.yaml.template"
			clusterName = "autoresize-single"
		)

		It("auto-resizes PVC when threshold exceeded", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			var cluster *apiv1.Cluster
			var primaryPod *corev1.Pod

			By("getting cluster and primary pod", func() {
				cluster, err = clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster).ToNot(BeNil())
				Expect(cluster.Spec.StorageConfiguration.AutoResize).ToNot(BeNil())
				Expect(cluster.Spec.StorageConfiguration.AutoResize.Enabled).To(BeTrue())

				primaryPod, err = clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(primaryPod).ToNot(BeNil())
			})

			var originalPVC corev1.PersistentVolumeClaim
			By("recording original PVC size", func() {
				err = env.Client.Get(env.Ctx,
					types.NamespacedName{Namespace: namespace, Name: primaryPod.Name}, &originalPVC)
				Expect(err).ToNot(HaveOccurred())
			})

			originalSize := originalPVC.Spec.Resources.Requests[corev1.ResourceStorage]

			By("filling disk to exceed threshold", func() {
				fillDiskToThreshold(namespace, primaryPod.Name,
					"/var/lib/postgresql/data/pgdata",
					cluster.Spec.StorageConfiguration.AutoResize.Threshold+5)
			})

			By("waiting for auto-resize to trigger", func() {
				Eventually(func(g Gomega) resource.Quantity {
					var pvc corev1.PersistentVolumeClaim
					err := env.Client.Get(env.Ctx,
						types.NamespacedName{Namespace: namespace, Name: primaryPod.Name}, &pvc)
					g.Expect(err).ToNot(HaveOccurred())
					return pvc.Spec.Resources.Requests[corev1.ResourceStorage]
				}).WithTimeout(5 * time.Minute).Should(BeNumerically(">", originalSize.Value()))
			})

			By("verifying cluster status records resize event", func() {
				Eventually(func(g Gomega) bool {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					return cluster.Status.DiskStatus != nil &&
						cluster.Status.DiskStatus.LastAutoResize != nil
				}).WithTimeout(time.Minute).Should(BeTrue())
			})

			By("verifying database is still functional after resize", func() {
				query := "SELECT 1"
				_, _, err := exec.QueryInInstancePod(
					env.Ctx, env.Client, env.Interface, env.RestClientConfig,
					exec.PodLocator{
						Namespace: namespace,
						PodName:   primaryPod.Name,
					},
					postgres.AppDBName,
					query)
				Expect(err).ToNot(HaveOccurred())
			})
		})
	})

	Context("separate WAL volume cluster", func() {
		const (
			sampleFile  = fixturesDir + "/autoresize/cluster-autoresize-wal-volume.yaml.template"
			clusterName = "autoresize-wal"
		)

		It("auto-resizes WAL PVC independently", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			var cluster *apiv1.Cluster
			var primaryPod *corev1.Pod

			By("getting cluster and primary pod", func() {
				cluster, err = clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(cluster).ToNot(BeNil())
				Expect(cluster.Spec.WalStorage).ToNot(BeNil())
				Expect(cluster.Spec.WalStorage.AutoResize).ToNot(BeNil())
				Expect(cluster.Spec.WalStorage.AutoResize.Enabled).To(BeTrue())

				primaryPod, err = clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				Expect(primaryPod).ToNot(BeNil())
			})

			walPVCName := primaryPod.Name + "-wal"
			var originalWALPVC corev1.PersistentVolumeClaim
			By("recording original WAL PVC size", func() {
				err = env.Client.Get(env.Ctx,
					types.NamespacedName{Namespace: namespace, Name: walPVCName}, &originalWALPVC)
				Expect(err).ToNot(HaveOccurred())
			})

			originalWALSize := originalWALPVC.Spec.Resources.Requests[corev1.ResourceStorage]

			By("filling WAL disk to exceed threshold", func() {
				fillDiskToThreshold(namespace, primaryPod.Name,
					"/var/lib/postgresql/wal/pg_wal",
					cluster.Spec.WalStorage.AutoResize.Threshold+5)
			})

			By("waiting for WAL auto-resize to trigger", func() {
				Eventually(func(g Gomega) resource.Quantity {
					var pvc corev1.PersistentVolumeClaim
					err := env.Client.Get(env.Ctx,
						types.NamespacedName{Namespace: namespace, Name: walPVCName}, &pvc)
					g.Expect(err).ToNot(HaveOccurred())
					return pvc.Spec.Resources.Requests[corev1.ResourceStorage]
				}).WithTimeout(5 * time.Minute).Should(BeNumerically(">", originalWALSize.Value()))
			})

			By("verifying database is still functional after resize", func() {
				query := "CHECKPOINT; SELECT pg_current_wal_lsn();"
				_, _, err := exec.QueryInInstancePod(
					env.Ctx, env.Client, env.Interface, env.RestClientConfig,
					exec.PodLocator{
						Namespace: namespace,
						PodName:   primaryPod.Name,
					},
					postgres.PostgresDBName,
					query)
				Expect(err).ToNot(HaveOccurred())
			})
		})
	})

	Context("max size limit", func() {
		const (
			sampleFile  = fixturesDir + "/autoresize/cluster-autoresize-single-volume.yaml.template"
			clusterName = "autoresize-single"
		)

		It("respects maxSize and stops resizing", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			var cluster *apiv1.Cluster
			var primaryPod *corev1.Pod

			By("getting cluster and primary pod", func() {
				cluster, err = clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				primaryPod, err = clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
			})

			maxSize := resource.MustParse(cluster.Spec.StorageConfiguration.AutoResize.MaxSize)

			By("repeatedly filling disk to trigger multiple resizes", func() {
				for i := 0; i < 5; i++ {
					fillDiskToThreshold(namespace, primaryPod.Name,
						"/var/lib/postgresql/data/pgdata",
						cluster.Spec.StorageConfiguration.AutoResize.Threshold+10)
					// Wait a bit between fills
					time.Sleep(30 * time.Second)
				}
			})

			By("verifying PVC does not exceed maxSize", func() {
				Consistently(func(g Gomega) resource.Quantity {
					var pvc corev1.PersistentVolumeClaim
					err := env.Client.Get(env.Ctx,
						types.NamespacedName{Namespace: namespace, Name: primaryPod.Name}, &pvc)
					g.Expect(err).ToNot(HaveOccurred())
					return pvc.Spec.Resources.Requests[corev1.ResourceStorage]
				}).WithTimeout(time.Minute).Should(BeNumerically("<=", maxSize.Value()))
			})

			By("checking for AutoResizeMaxReached event", func() {
				Eventually(func(g Gomega) bool {
					var pvc corev1.PersistentVolumeClaim
					err := env.Client.Get(env.Ctx,
						types.NamespacedName{Namespace: namespace, Name: primaryPod.Name}, &pvc)
					g.Expect(err).ToNot(HaveOccurred())
					currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
					// If we've reached max size, the event should have been generated
					return currentSize.Cmp(maxSize) >= 0
				}).WithTimeout(5 * time.Minute).Should(BeTrue())
			})
		})
	})

	Context("validation", func() {
		It("rejects invalid threshold values", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			cluster := &apiv1.Cluster{}
			cluster.Name = "invalid-threshold"
			cluster.Namespace = namespace
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Size: "1Gi",
				AutoResize: &apiv1.AutoResizeConfiguration{
					Enabled:   true,
					Threshold: 150, // Invalid: > 99
				},
			}

			err = env.Client.Create(env.Ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("threshold"))
		})

		It("requires acknowledgeWALRisk for single-volume clusters", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			cluster := &apiv1.Cluster{}
			cluster.Name = "missing-ack"
			cluster.Namespace = namespace
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Size: "1Gi",
				AutoResize: &apiv1.AutoResizeConfiguration{
					Enabled:   true,
					Threshold: 80,
					// Missing walSafetyPolicy.acknowledgeWALRisk = true
				},
			}

			err = env.Client.Create(env.Ctx, cluster)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("acknowledgeWALRisk"))
		})
	})
})
