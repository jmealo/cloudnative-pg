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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	"github.com/cloudnative-pg/cloudnative-pg/tests"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/backups"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/clusterutils"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/nodes"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/timeouts"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Dynamic storage management extended scenarios",
	Serial, Label(tests.LabelStorage, "dynamic-storage-p1"), func() {
		const (
			level           = tests.High
			namespacePrefix = "dynamic-storage-p1"
		)

		BeforeEach(func() {
			if testLevelEnv.Depth < int(level) {
				Skip("Test depth is lower than the amount requested for this test")
			}
			if MustGetEnvProfile().UsesNodeDiskSpace() {
				Skip("this test requires dynamic volume provisioning with resize support")
			}
		})

		Context("Extended Scenarios", func() {
			var namespace string

			BeforeEach(func() {
				var err error
				namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
				Expect(err).ToNot(HaveOccurred())
			})

			It("verify that concurrent backup, node drain and in-flight growth work correctly", func() {
				nodesList, _ := nodes.List(env.Ctx, env.Client)
				if len(nodesList.Items) < 2 {
					Skip("This test requires at least 2 nodes")
				}

				clusterName := "ds-011"
				cluster := &apiv1.Cluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      clusterName,
						Namespace: namespace,
					},
					Spec: apiv1.ClusterSpec{
						Instances: 3,
						StorageConfiguration: apiv1.StorageConfiguration{
							Request:      "5Gi",
							Limit:        "20Gi",
							TargetBuffer: ptr.To(20),
							MaintenanceWindow: &apiv1.MaintenanceWindowConfig{
								// Use December 31st (valid) instead of February 31st (invalid)
								Schedule: "0 0 4 31 12 *",
							},
							// Set high emergency thresholds to prevent emergency growth
							// which can cause timeouts with concurrent operations
							EmergencyGrow: &apiv1.EmergencyGrowConfig{
								CriticalThreshold:   99,
								CriticalMinimumFree: "100Mi",
							},
						},
					},
				}
				tableLocator := TableLocator{
					Namespace:    namespace,
					ClusterName:  clusterName,
					DatabaseName: "app",
					TableName:    "sentinel",
				}

				By("creating cluster", func() {
					err := env.Client.Create(env.Ctx, cluster)
					Expect(err).ToNot(HaveOccurred())
					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
				})

				By("inserting sentinel data", func() {
					AssertCreateTestData(env, tableLocator)
				})

				By("triggering growth condition", func() {
					primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
					Expect(err).ToNot(HaveOccurred())
					_, err = fillDiskIncrementally(primaryPod, 85, 90, 500000)
					Expect(err).ToNot(HaveOccurred())
				})

				By("waiting for storage sizing to detect growth need", func() {
					Eventually(func(g Gomega) {
						cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
						g.Expect(err).ToNot(HaveOccurred())
						g.Expect(cluster.Status.StorageSizing).ToNot(BeNil(),
							"Expected StorageSizing status to be populated before concurrent operations")
					}).WithTimeout(time.Duration(testTimeouts[timeouts.StorageSizingDetection]) * time.Second).
						WithPolling(time.Duration(testTimeouts[timeouts.StorageSizingPolling]) * time.Second).Should(Succeed())
				})

				backupName := clusterName + "-concurrent-backup"
				By("starting concurrent operations", func() {
					// 1. Create backup
					backup := &apiv1.Backup{
						ObjectMeta: metav1.ObjectMeta{
							Name:      backupName,
							Namespace: namespace,
						},
						Spec: apiv1.BackupSpec{
							Cluster: apiv1.LocalObjectReference{
								Name: clusterName,
							},
						},
					}
					err := env.Client.Create(env.Ctx, backup)
					Expect(err).ToNot(HaveOccurred())

					// 2. Drain node
					podsOnPrimaryNode := nodes.DrainPrimary(
						env.Ctx, env.Client, namespace, clusterName, testTimeouts[timeouts.DrainNode],
					)
					Expect(podsOnPrimaryNode).ToNot(BeEmpty())
					err = nodes.UncordonAll(env.Ctx, env.Client)
					Expect(err).ToNot(HaveOccurred())

					// 3. Open maintenance window
					updateCluster(namespace, clusterName, func(cluster *apiv1.Cluster) {
						cluster.Spec.StorageConfiguration.MaintenanceWindow = nil
					})
				})

				By("verifying backup reaches a terminal phase", func() {
					Eventually(func(g Gomega) {
						backup := &apiv1.Backup{}
						getErr := env.Client.Get(env.Ctx, ctrlclient.ObjectKey{
							Namespace: namespace,
							Name:      backupName,
						}, backup)
						g.Expect(getErr).ToNot(HaveOccurred())
						// Use BeEquivalentTo for type-safe comparison with BackupPhase type alias
						g.Expect(backup.Status.Phase).To(Or(
							BeEquivalentTo(apiv1.BackupPhaseCompleted),
							BeEquivalentTo(apiv1.BackupPhaseFailed),
						))
					}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSBackupIsReady]) * time.Second).
						WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
				})

				By("verifying eventual convergence", func() {
					verifyGrowthCompletion(namespace, clusterName)
					// Use ClusterIsReadySlow because node drain + concurrent backup + volume operations can take 20+ minutes
					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReadySlow], env)
					AssertDataExpectedCount(env, tableLocator, 2)
				})
			})

			It("verify that rolling image upgrade while dynamic sizing operation is active works correctly", func() {
				clusterName := "ds-012"
				cluster := &apiv1.Cluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      clusterName,
						Namespace: namespace,
					},
					Spec: apiv1.ClusterSpec{
						Instances: 3,
						StorageConfiguration: apiv1.StorageConfiguration{
							Request:      "5Gi",
							Limit:        "20Gi",
							TargetBuffer: ptr.To(20),
							MaintenanceWindow: &apiv1.MaintenanceWindowConfig{
								// Use December 31st (valid) instead of February 31st (invalid)
								Schedule: "0 0 4 31 12 *",
							},
							// Set high emergency thresholds to prevent emergency growth
							EmergencyGrow: &apiv1.EmergencyGrowConfig{
								CriticalThreshold:   99,
								CriticalMinimumFree: "100Mi",
							},
						},
					},
				}
				tableLocator := TableLocator{
					Namespace:    namespace,
					ClusterName:  clusterName,
					DatabaseName: "app",
					TableName:    "sentinel",
				}
				initialPodUIDs := make(map[string]types.UID)

				By("creating cluster", func() {
					err := env.Client.Create(env.Ctx, cluster)
					Expect(err).ToNot(HaveOccurred())
					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
				})

				By("inserting sentinel data", func() {
					AssertCreateTestData(env, tableLocator)
				})

				By("recording initial pod identities", func() {
					podList, err := clusterutils.ListPods(env.Ctx, env.Client, namespace, clusterName)
					Expect(err).ToNot(HaveOccurred())
					for _, pod := range podList.Items {
						initialPodUIDs[pod.Name] = pod.UID
					}
				})

				By("triggering growth condition", func() {
					primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
					Expect(err).ToNot(HaveOccurred())
					_, err = fillDiskIncrementally(primaryPod, 85, 90, 500000)
					Expect(err).ToNot(HaveOccurred())
				})

				By("triggering rolling upgrade while growth is pending", func() {
					updateCluster(namespace, clusterName, func(cluster *apiv1.Cluster) {
						cluster.Spec.Env = append(cluster.Spec.Env, corev1.EnvVar{
							Name:  "TRIGGER_ROLLOUT",
							Value: "true",
						})
					})
				})

				By("opening maintenance window", func() {
					updateCluster(namespace, clusterName, func(cluster *apiv1.Cluster) {
						cluster.Spec.StorageConfiguration.MaintenanceWindow = nil
					})
				})

				By("verifying both operations complete", func() {
					verifyGrowthCompletion(namespace, clusterName)
					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
					AssertDataExpectedCount(env, tableLocator, 2)
				})

				By("verifying rolling upgrade replaced at least one pod", func() {
					podList, err := clusterutils.ListPods(env.Ctx, env.Client, namespace, clusterName)
					Expect(err).ToNot(HaveOccurred())

					replacedPods := 0
					for _, pod := range podList.Items {
						if oldUID, ok := initialPodUIDs[pod.Name]; ok && oldUID != pod.UID {
							replacedPods++
						}
					}
					Expect(replacedPods).To(BeNumerically(">=", 1),
						"expected at least one pod replacement from rolling upgrade")
				})
			})

			It("verify that volume snapshot creation around dynamically resized volumes works correctly", func() {
				if !utils.HaveVolumeSnapshot() {
					Skip("This test requires VolumeSnapshot support")
				}

				clusterName := "ds-013"
				cluster := &apiv1.Cluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      clusterName,
						Namespace: namespace,
					},
					Spec: apiv1.ClusterSpec{
						Instances: 1,
						StorageConfiguration: apiv1.StorageConfiguration{
							Request:      "5Gi",
							Limit:        "20Gi",
							TargetBuffer: ptr.To(20),
						},
					},
				}

				By("creating cluster", func() {
					err := env.Client.Create(env.Ctx, cluster)
					Expect(err).ToNot(HaveOccurred())
					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
				})

				By("triggering growth", func() {
					primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
					Expect(err).ToNot(HaveOccurred())
					_, err = fillDiskIncrementally(primaryPod, 85, 90, 500000)
					Expect(err).ToNot(HaveOccurred())
					verifyGrowthCompletion(namespace, clusterName)
				})

				By("verifying snapshots can be taken of the resized volume", func() {
					backupName := clusterName + "-snapshot"
					err := backups.CreateOnDemandBackupViaKubectlPlugin(
						namespace,
						clusterName,
						backupName,
						apiv1.BackupTargetStandby,
						apiv1.BackupMethodVolumeSnapshot,
					)
					Expect(err).ToNot(HaveOccurred())

					Eventually(func(g Gomega) {
						backup := &apiv1.Backup{}
						getErr := env.Client.Get(env.Ctx, ctrlclient.ObjectKey{
							Namespace: namespace,
							Name:      backupName,
						}, backup)
						g.Expect(getErr).ToNot(HaveOccurred())
						g.Expect(backup.Status.Phase).To(BeEquivalentTo(apiv1.BackupPhaseCompleted))
					}, testTimeouts[timeouts.VolumeSnapshotIsReady]).WithPolling(
						time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second,
					).Should(Succeed())
				})
			})

			It("verify that post-growth steady state does not cause resize flapping", func() {
				clusterName := "ds-014"
				cluster := &apiv1.Cluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      clusterName,
						Namespace: namespace,
					},
					Spec: apiv1.ClusterSpec{
						Instances: 1,
						StorageConfiguration: apiv1.StorageConfiguration{
							Request:      "5Gi",
							Limit:        "20Gi",
							TargetBuffer: ptr.To(20),
						},
					},
				}

				By("creating cluster", func() {
					err := env.Client.Create(env.Ctx, cluster)
					Expect(err).ToNot(HaveOccurred())
					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
				})

				By("triggering growth", func() {
					primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
					Expect(err).ToNot(HaveOccurred())
					_, err = fillDiskIncrementally(primaryPod, 85, 90, 500000)
					Expect(err).ToNot(HaveOccurred())
					verifyGrowthCompletion(namespace, clusterName)
				})

				var stableSize resource.Quantity
				By("recording stabilized PVC size after growth", func() {
					var pvcList corev1.PersistentVolumeClaimList
					err := env.Client.List(env.Ctx, &pvcList,
						ctrlclient.InNamespace(namespace),
						ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
					Expect(err).ToNot(HaveOccurred())
					Expect(pvcList.Items).To(HaveLen(1))
					stableSize = pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
				})

				By("verifying no resize flapping after convergence", func() {
					Consistently(func(g Gomega) {
						var pvcList corev1.PersistentVolumeClaimList
						err := env.Client.List(env.Ctx, &pvcList,
							ctrlclient.InNamespace(namespace),
							ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
						g.Expect(err).ToNot(HaveOccurred())
						g.Expect(pvcList.Items).To(HaveLen(1))
						currentSize := pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
						g.Expect(currentSize.Cmp(stableSize)).To(BeZero())
					}).WithTimeout(time.Minute * 2).WithPolling(10 * time.Second).Should(Succeed())
				})
			})
		})
	})
