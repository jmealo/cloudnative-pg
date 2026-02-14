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

SPDX-License-Identifier: Apache-2.0
*/

package e2e

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/tests"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/clusterutils"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/exec"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/postgres"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/yaml"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Tests for logical slot cleanup after switchover when synchronizeLogicalDecoding is enabled
var _ = Describe("Logical Slot Switchover", Label(tests.LabelPublicationSubscription, tests.LabelSelfHealing), func() {
	const (
		sourceClusterManifest       = fixturesDir + "/logical_slot_switchover/source-cluster.yaml.template"
		destinationClusterManifest  = fixturesDir + "/logical_slot_switchover/destination-cluster.yaml.template"
		sourceDatabaseManifest      = fixturesDir + "/logical_slot_switchover/source-database.yaml"
		destinationDatabaseManifest = fixturesDir + "/logical_slot_switchover/destination-database.yaml"
		pubManifest                 = fixturesDir + "/logical_slot_switchover/pub.yaml"
		subManifest                 = fixturesDir + "/logical_slot_switchover/sub.yaml"
		level                       = tests.High
	)

	BeforeEach(func() {
		if testLevelEnv.Depth < int(level) {
			Skip("Test depth is lower than the amount requested for this test")
		}
	})

	Context("with synchronizeLogicalDecoding enabled on PG17+", Ordered, func() {
		const (
			namespacePrefix = "logical-slot-switchover"
			dbname          = "testdb"
			tableName       = "test_data"
		)
		var (
			sourceClusterName, destinationClusterName, namespace string
			err                                                  error
		)

		BeforeAll(func() {
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			sourceClusterName, err = yaml.GetResourceNameFromYAML(env.Scheme, sourceClusterManifest)
			Expect(err).ToNot(HaveOccurred())

			destinationClusterName, err = yaml.GetResourceNameFromYAML(env.Scheme, destinationClusterManifest)
			Expect(err).ToNot(HaveOccurred())

			By("setting up source cluster with synchronizeLogicalDecoding", func() {
				AssertCreateCluster(namespace, sourceClusterName, sourceClusterManifest, env)
			})

			By("setting up destination cluster", func() {
				AssertCreateCluster(namespace, destinationClusterName, destinationClusterManifest, env)
			})

			By("creating databases", func() {
				CreateResourceFromFile(namespace, sourceDatabaseManifest)
				CreateResourceFromFile(namespace, destinationDatabaseManifest)

				// Wait for databases to be ready
				Eventually(func(g Gomega) {
					db := &apiv1.Database{}
					err := env.Client.Get(env.Ctx, types.NamespacedName{Namespace: namespace, Name: "source-db"}, db)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(db.Status.Applied).Should(HaveValue(BeTrue()))
				}, 300).WithPolling(10 * time.Second).Should(Succeed())

				Eventually(func(g Gomega) {
					db := &apiv1.Database{}
					err := env.Client.Get(env.Ctx, types.NamespacedName{Namespace: namespace, Name: "dest-db"}, db)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(db.Status.Applied).Should(HaveValue(BeTrue()))
				}, 300).WithPolling(10 * time.Second).Should(Succeed())
			})

			By("creating test table on source", func() {
				query := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (id SERIAL PRIMARY KEY, data TEXT)", tableName)
				_, err = postgres.RunExecOverForward(
					env.Ctx, env.Client, env.Interface, env.RestClientConfig,
					namespace, sourceClusterName, dbname,
					apiv1.ApplicationUserSecretSuffix, query,
				)
				Expect(err).ToNot(HaveOccurred())

				// Insert initial test data
				_, err = postgres.RunExecOverForward(
					env.Ctx, env.Client, env.Interface, env.RestClientConfig,
					namespace, sourceClusterName, dbname,
					apiv1.ApplicationUserSecretSuffix, "INSERT INTO test_data (data) VALUES ('before_switchover')",
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("creating test table on destination", func() {
				query := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (id SERIAL PRIMARY KEY, data TEXT)", tableName)
				_, err = postgres.RunExecOverForward(
					env.Ctx, env.Client, env.Interface, env.RestClientConfig,
					namespace, destinationClusterName, dbname,
					apiv1.ApplicationUserSecretSuffix, query,
				)
				Expect(err).ToNot(HaveOccurred())
			})

			By("setting up publication and subscription", func() {
				CreateResourceFromFile(namespace, pubManifest)
				CreateResourceFromFile(namespace, subManifest)

				// Wait for pub/sub to be ready
				Eventually(func(g Gomega) {
					pub := &apiv1.Publication{}
					err := env.Client.Get(env.Ctx, types.NamespacedName{Namespace: namespace, Name: "test-pub"}, pub)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(pub.Status.Applied).Should(HaveValue(BeTrue()))
				}, 300).WithPolling(10 * time.Second).Should(Succeed())

				Eventually(func(g Gomega) {
					sub := &apiv1.Subscription{}
					err := env.Client.Get(env.Ctx, types.NamespacedName{Namespace: namespace, Name: "test-sub"}, sub)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(sub.Status.Applied).Should(HaveValue(BeTrue()))
				}, 300).WithPolling(10 * time.Second).Should(Succeed())
			})
		})

		It("cleans up orphaned logical slots after switchover", func() {
			var oldPrimary string

			By("recording initial primary", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, sourceClusterName)
				Expect(err).ToNot(HaveOccurred())
				oldPrimary = cluster.Status.CurrentPrimary
			})

			By("verifying logical slots exist on primary", func() {
				primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, sourceClusterName)
				Expect(err).ToNot(HaveOccurred())

				query := "SELECT count(*) FROM pg_replication_slots WHERE slot_type = 'logical'"
				Eventually(func(g Gomega) {
					out, _, err := exec.QueryInInstancePod(
						env.Ctx, env.Client, env.Interface, env.RestClientConfig,
						exec.PodLocator{Namespace: primaryPod.Namespace, PodName: primaryPod.Name},
						dbname, query,
					)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(strings.TrimSpace(out)).ToNot(Equal("0"))
				}, 60).Should(Succeed())
			})

			By("triggering switchover", func() {
				AssertSwitchover(namespace, sourceClusterName, env)
			})

			By("verifying no synced=false slots on demoted primary", func() {
				// The old primary is now a replica
				query := "SELECT count(*) FROM pg_replication_slots WHERE slot_type = 'logical' AND synced = false"

				Eventually(func(g Gomega) {
					out, _, err := exec.QueryInInstancePod(
						env.Ctx, env.Client, env.Interface, env.RestClientConfig,
						exec.PodLocator{Namespace: namespace, PodName: oldPrimary},
						postgres.PostgresDBName, query,
					)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(strings.TrimSpace(out)).To(Equal("0"), "Expected no synced=false logical slots on demoted primary")
				}, 120).WithPolling(5 * time.Second).Should(Succeed())
			})

			By("verifying logical replication still works", func() {
				// Insert new data after switchover
				_, err = postgres.RunExecOverForward(
					env.Ctx, env.Client, env.Interface, env.RestClientConfig,
					namespace, sourceClusterName, dbname,
					apiv1.ApplicationUserSecretSuffix, "INSERT INTO test_data (data) VALUES ('after_switchover')",
				)
				Expect(err).ToNot(HaveOccurred())

				// Verify it appears on destination
				Eventually(func(g Gomega) {
					row, err := postgres.RunQueryRowOverForward(
						env.Ctx, env.Client, env.Interface, env.RestClientConfig,
						namespace, destinationClusterName, dbname,
						apiv1.ApplicationUserSecretSuffix,
						"SELECT count(*) FROM test_data WHERE data = 'after_switchover'",
					)
					g.Expect(err).ToNot(HaveOccurred())

					var count int
					err = row.Scan(&count)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(count).To(BeNumerically(">", 0))
				}, 120).WithPolling(5 * time.Second).Should(Succeed())
			})
		})
	})
})
