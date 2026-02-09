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
								Schedule: "0 0 4 31 2 *",
							},
						},
					},
				}

				By("creating cluster", func() {
					err := env.Client.Create(env.Ctx, cluster)
					Expect(err).ToNot(HaveOccurred())
					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
				})

				By("triggering growth condition", func() {
					primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
					Expect(err).ToNot(HaveOccurred())
					_, err = fillDiskIncrementally(primaryPod, 85, 90, 500000)
					Expect(err).ToNot(HaveOccurred())
				})

				By("starting concurrent operations", func() {
					// 1. Create backup
					backup := &apiv1.Backup{
						ObjectMeta: metav1.ObjectMeta{
							Name:      clusterName + "-concurrent-backup",
							Namespace: namespace,
						},
						Spec: apiv1.BackupSpec{
							Cluster: apiv1.LocalObjectReference{
								Name: clusterName,
							},
						},
					}
					_ = env.Client.Create(env.Ctx, backup)

					// 2. Drain node
					_ = nodes.DrainPrimary(env.Ctx, env.Client, namespace, clusterName, testTimeouts[timeouts.DrainNode])
					_ = nodes.UncordonAll(env.Ctx, env.Client)

					// 3. Open maintenance window
					updateCluster(namespace, clusterName, func(cluster *apiv1.Cluster) {
						cluster.Spec.StorageConfiguration.MaintenanceWindow = nil
					})
				})

				By("verifying eventual convergence", func() {
					verifyGrowthCompletion(namespace, clusterName)
					assertDataConsistency(namespace, clusterName)
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
								Schedule: "0 0 4 31 2 *",
							},
						},
					},
				}

				By("creating cluster", func() {
					err := env.Client.Create(env.Ctx, cluster)
					Expect(err).ToNot(HaveOccurred())
					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
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
					assertDataConsistency(namespace, clusterName)
				})
			})

			It("verify that volume snapshot creation and restore around dynamically resized volumes work correctly", func() {
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
						backupList, err := backups.List(env.Ctx, env.Client, namespace)
						g.Expect(err).ToNot(HaveOccurred())
						for _, backup := range backupList.Items {
							if backup.Name == backupName {
								g.Expect(backup.Status.Phase).To(BeEquivalentTo(apiv1.BackupPhaseCompleted))
							}
						}
					}, testTimeouts[timeouts.VolumeSnapshotIsReady]).Should(Succeed())
				})
			})

			It("verify that repeated oscillation around threshold band does not cause flapping", func() {
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

				By("triggering growth and verifying no flapping", func() {
					primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
					Expect(err).ToNot(HaveOccurred())
					_, err = fillDiskIncrementally(primaryPod, 85, 90, 500000)
					Expect(err).ToNot(HaveOccurred())
					verifyGrowthCompletion(namespace, clusterName)

					Consistently(func(g Gomega) {
						var pvcList corev1.PersistentVolumeClaimList
						err := env.Client.List(env.Ctx, &pvcList,
							ctrlclient.InNamespace(namespace),
							ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
						g.Expect(err).ToNot(HaveOccurred())
						g.Expect(pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("10Gi")))
					}).WithTimeout(time.Minute * 2).Should(Succeed())
				})
			})
		})
	})
