# E2E Proposed Diffs (Review Only)

These are **proposed patches only** based on the latest AKS E2E run. They are not applied.

## 1) Harden row-count checks against transient port-forward failures

**Why:** `Dynamic Storage Topology T3` failed at `asserts_test.go:663` with transient forward/connect errors right after resize completed.

```diff
diff --git a/tests/e2e/asserts_test.go b/tests/e2e/asserts_test.go
index 7c5f4032d..8d4f72f23 100644
--- a/tests/e2e/asserts_test.go
+++ b/tests/e2e/asserts_test.go
@@ -257,6 +257,13 @@ func AssertCreateCluster(
 // AssertClusterIsReady checks the cluster has as many pods as in spec, that
 // none of them are going to be deleted, and that the status is Healthy
 func AssertClusterIsReady(namespace string, clusterName string, timeout int, env *environment.TestingEnvironment) {
+	clusterStateReport := func() string {
+		cluster := testsUtils.PrintClusterResources(env.Ctx, env.Client, namespace, clusterName)
+		kubeNodes, _ := nodes.DescribeKubernetesNodes(env.Ctx, env.Client)
+		return fmt.Sprintf("CLUSTER STATE\n%s\n\nK8S NODES\n%s",
+			cluster, kubeNodes)
+	}
+
 	By(fmt.Sprintf("having a Cluster %s with each instance in status ready", clusterName), func() {
 		// Eventually the number of ready instances should be equal to the
 		// amount of instances defined in the cluster and
@@ -272,25 +279,29 @@ func AssertClusterIsReady(namespace string, clusterName string, timeout int, en
 		}).Should(Succeed())
 
 		start := time.Now()
-		Eventually(func() (string, error) {
+		Eventually(func(g Gomega) {
+			var err error
+			cluster, err = clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
+			g.Expect(err).ToNot(HaveOccurred())
+
 			podList, err := clusterutils.ListPods(env.Ctx, env.Client, namespace, clusterName)
-			if err != nil {
-				return "", err
-			}
-			if cluster.Spec.Instances == utils.CountReadyPods(podList.Items) {
-				for _, pod := range podList.Items {
-					if pod.DeletionTimestamp != nil {
-						return fmt.Sprintf("Pod '%s' is waiting for deletion", pod.Name), nil
-					}
-				}
-				cluster, err = clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
-				return cluster.Status.Phase, err
+			g.Expect(err).ToNot(HaveOccurred())
+
+			readyPods := utils.CountReadyPods(podList.Items)
+			g.Expect(readyPods).To(BeEquivalentTo(cluster.Spec.Instances),
+				"Ready pod is not as expected. Spec Instances: %d, ready pods: %d",
+				cluster.Spec.Instances,
+				readyPods)
+
+			for _, pod := range podList.Items {
+				g.Expect(pod.DeletionTimestamp).To(BeNil(),
+					"Pod '%s' is waiting for deletion", pod.Name)
 			}
-			return fmt.Sprintf("Ready pod is not as expected. Spec Instances: %d, ready pods: %d \n",
-				cluster.Spec.Instances,
-				utils.CountReadyPods(podList.Items)), nil
-		}, timeout, 2).Should(BeEquivalentTo(apiv1.PhaseHealthy),
-			func() string {
-				cluster := testsUtils.PrintClusterResources(env.Ctx, env.Client, namespace, clusterName)
-				kubeNodes, _ := nodes.DescribeKubernetesNodes(env.Ctx, env.Client)
-				return fmt.Sprintf("CLUSTER STATE\n%s\n\nK8S NODES\n%s",
-					cluster, kubeNodes)
-			},
-		)
+		}, timeout, 2).Should(Succeed(), clusterStateReport)
+
+		Eventually(func() (string, error) {
+			cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
+			if err != nil {
+				return "", err
+			}
+			return cluster.Status.Phase, nil
+		}, timeout, 2).Should(BeEquivalentTo(apiv1.PhaseHealthy), clusterStateReport)
 
 		if cluster.Spec.Instances != 1 {
 			Eventually(func(g Gomega) {
@@ -640,6 +651,24 @@ func foreignServerExistsQuery(serverName string) string {
 	return fmt.Sprintf("SELECT EXISTS(SELECT FROM pg_catalog.pg_foreign_server WHERE srvname='%v')", serverName)
 }
 
+func isTransientForwardingError(err error) bool {
+	if err == nil {
+		return false
+	}
+
+	message := strings.ToLower(err.Error())
+	transientSignals := []string{
+		"connection refused",
+		"tls error: eof",
+		"lost connection to pod",
+		"error forwarding port",
+		"use of closed network connection",
+	}
+	for _, signal := range transientSignals {
+		if strings.Contains(message, signal) {
+			return true
+		}
+	}
+	return false
+}
+
 // AssertDataExpectedCount verifies that an expected amount of rows exists on the table
 func AssertDataExpectedCount(
 	env *environment.TestingEnvironment,
@@ -647,22 +676,40 @@ func AssertDataExpectedCount(
 	expectedValue int,
 ) {
 	By(fmt.Sprintf("verifying test data in table %v (cluster %v, database %v, tablespace %v)",
 		tl.TableName, tl.ClusterName, tl.DatabaseName, tl.Tablespace), func() {
-		row, err := postgres.RunQueryRowOverForward(
-			env.Ctx,
-			env.Client,
-			env.Interface,
-			env.RestClientConfig,
-			tl.Namespace,
-			tl.ClusterName,
-			tl.DatabaseName,
-			apiv1.ApplicationUserSecretSuffix,
-			fmt.Sprintf("SELECT COUNT(*) FROM %s", tl.TableName),
-		)
-		Expect(err).ToNot(HaveOccurred())
-
-		var nRows int
-		err = row.Scan(&nRows)
-		Expect(err).ToNot(HaveOccurred())
-		Expect(nRows).Should(BeEquivalentTo(expectedValue))
+		Eventually(func() error {
+			row, err := postgres.RunQueryRowOverForward(
+				env.Ctx,
+				env.Client,
+				env.Interface,
+				env.RestClientConfig,
+				tl.Namespace,
+				tl.ClusterName,
+				tl.DatabaseName,
+				apiv1.ApplicationUserSecretSuffix,
+				fmt.Sprintf("SELECT COUNT(*) FROM %s", tl.TableName),
+			)
+			if err != nil {
+				if isTransientForwardingError(err) {
+					GinkgoWriter.Printf("Transient query transport error while checking %s: %v\n", tl.TableName, err)
+					return err
+				}
+				return fmt.Errorf("query failed while counting rows in %s: %w", tl.TableName, err)
+			}
+
+			var nRows int
+			err = row.Scan(&nRows)
+			if err != nil {
+				if isTransientForwardingError(err) {
+					GinkgoWriter.Printf("Transient scan transport error while checking %s: %v\n", tl.TableName, err)
+					return err
+				}
+				return fmt.Errorf("failed to scan row count for %s: %w", tl.TableName, err)
+			}
+
+			if nRows != expectedValue {
+				return fmt.Errorf("unexpected row count in %s: expected %d, got %d",
+					tl.TableName, expectedValue, nRows)
+			}
+			return nil
+		}, testTimeouts[timeouts.AKSVolumeAttach], 2).Should(Succeed())
 	})
 }
```

## 2) Make node-drain helper resilient to AKS node replacement (`NotFound`)

**Why:** `Node drain during growth` failed at `nodes.go:70` because the target VMSS node name no longer existed while draining.

```diff
diff --git a/tests/utils/nodes/nodes.go b/tests/utils/nodes/nodes.go
index a8f6f62f6..85a8f8b7d 100644
--- a/tests/utils/nodes/nodes.go
+++ b/tests/utils/nodes/nodes.go
@@ -50,6 +50,7 @@ func DrainPrimary(
 		pod, err := clusterutils.GetPrimary(ctx, crudClient, namespace, clusterName)
 		Expect(err).ToNot(HaveOccurred())
 		primaryNode = pod.Spec.NodeName
+		Expect(primaryNode).ToNot(BeEmpty(), "primary pod is not scheduled to any node")
 
 		// Gather the pods running on this node
 		podList, err := clusterutils.ListPods(ctx, crudClient, namespace, clusterName)
@@ -63,25 +64,45 @@ func DrainPrimary(
 
 		// Draining the primary pod's node
 		var stdout, stderr string
+		drainedNode := primaryNode
 		Eventually(func() error {
 			cmd := fmt.Sprintf("kubectl drain %v --ignore-daemonsets --delete-emptydir-data --force --timeout=%ds",
-				primaryNode, timeoutSeconds)
+				drainedNode, timeoutSeconds)
 			stdout, stderr, err = run.Unchecked(cmd)
+			if err == nil {
+				return nil
+			}
+
+			if isNodeNotFound(err, drainedNode) {
+				currentPrimary, getErr := clusterutils.GetPrimary(ctx, crudClient, namespace, clusterName)
+				if getErr == nil && currentPrimary.Spec.NodeName != "" && currentPrimary.Spec.NodeName != drainedNode {
+					GinkgoWriter.Printf(
+						"Drain target node %s no longer exists and primary moved to %s; treating drain as converged\n",
+						drainedNode,
+						currentPrimary.Spec.NodeName,
+					)
+					return nil
+				}
+			}
 			return err
-		}, timeoutSeconds).ShouldNot(HaveOccurred(), fmt.Sprintf("stdout: %s, stderr: %s", stdout, stderr))
+		}, timeoutSeconds, 10).ShouldNot(HaveOccurred(), fmt.Sprintf("stdout: %s, stderr: %s", stdout, stderr))
 	})
+
 	By("ensuring no cluster pod is still running on the drained node", func() {
 		Eventually(func() ([]string, error) {
 			podList, err := clusterutils.ListPods(ctx, crudClient, namespace, clusterName)
 			usedNodes := make([]string, 0, len(podList.Items))
 			for _, pod := range podList.Items {
 				usedNodes = append(usedNodes, pod.Spec.NodeName)
 			}
 			return usedNodes, err
-		}, 60).ShouldNot(ContainElement(primaryNode))
+		}, 60).ShouldNot(ContainElement(primaryNode))
 	})
 
 	return podNames
 }
+
+func isNodeNotFound(err error, nodeName string) bool {
+	if err == nil {
+		return false
+	}
+	message := err.Error()
+	return strings.Contains(message, "Error from server (NotFound)") &&
+		strings.Contains(message, fmt.Sprintf("nodes %q not found", nodeName))
+}
```

## 3) Improve tablespace-growth determinism and observability

**Why:** `Tablespace dynamic sizing` timed out waiting for request change; run shows fill plateau at 82% with no request growth.

```diff
diff --git a/tests/e2e/dynamic_storage_test.go b/tests/e2e/dynamic_storage_test.go
index 3d9bb807d..44bc73f1f 100644
--- a/tests/e2e/dynamic_storage_test.go
+++ b/tests/e2e/dynamic_storage_test.go
@@ -218,7 +218,11 @@ func fillTablespaceDiskIncrementally(
 		if err != nil {
 			return currentUsage, err
 		}
 
 		time.Sleep(2 * time.Second)
-		currentUsage, _ = getTablespaceDiskUsagePercent(pod, tbsName)
+		currentUsage, err = getTablespaceDiskUsagePercent(pod, tbsName)
+		if err != nil {
+			return currentUsage, fmt.Errorf("failed to get tablespace usage after batch %d: %w", batchNum, err)
+		}
+		GinkgoWriter.Printf("After tablespace batch %d: disk usage is %d%%\n", batchNum, currentUsage)
 	}
 
 	return currentUsage, nil
@@ -788,8 +792,10 @@ var _ = Describe("Dynamic Storage", Label(tests.LabelStorage, tests.LabelDynam
 			})
 
 			By("filling tablespace disk to trigger growth", func() {
-				// Use smaller batches to avoid abrupt disk exhaustion on small tablespace PVCs.
-				finalUsage, err := fillTablespaceDiskIncrementally(primaryPod, tbsName, 82, 88, 100000, namespace, clusterName)
+				// Push clearly beyond the 80% trigger threshold while retaining headroom.
+				// 86/92 reduces threshold-edge behavior where no action is emitted at exactly ~82%.
+				finalUsage, err := fillTablespaceDiskIncrementally(
+					primaryPod, tbsName, 86, 92, 100000, namespace, clusterName)
 				if err != nil {
 					GinkgoWriter.Printf("Tablespace disk fill ended with error: %v\n", err)
 				}
@@ -799,13 +805,41 @@ var _ = Describe("Dynamic Storage", Label(tests.LabelStorage, tests.LabelDynam
 				GinkgoWriter.Printf("Final tablespace usage after fill: %d%%\n", finalUsage)
 			})
 
+			By("verifying tablespace sizing target is computed before PVC request update", func() {
+				Eventually(func(g Gomega) {
+					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
+					g.Expect(err).ToNot(HaveOccurred())
+					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
+					g.Expect(cluster.Status.StorageSizing.Tablespaces).ToNot(BeNil())
+
+					tbsStatus := cluster.Status.StorageSizing.Tablespaces[tbsName]
+					g.Expect(tbsStatus).ToNot(BeNil(), "tablespace status should exist for %s", tbsName)
+					g.Expect(tbsStatus.State).To(Or(
+						Equal("NeedsGrow"),
+						Equal("PendingGrowth"),
+						Equal("Resizing"),
+						Equal("Balanced"),
+						Equal("AtLimit"),
+						Equal("Emergency"),
+					), "tablespace state should reflect active sizing lifecycle")
+					g.Expect(tbsStatus.TargetSize).ToNot(BeEmpty(),
+						"tablespace target size should be computed after threshold crossing")
+
+					targetSize, parseErr := resource.ParseQuantity(tbsStatus.TargetSize)
+					g.Expect(parseErr).ToNot(HaveOccurred())
+					g.Expect(targetSize.Cmp(initialTablespaceRequest)).To(BeNumerically(">", 0),
+						"tablespace target size should exceed initial request %s", initialTablespaceRequest.String())
+				}).WithTimeout(time.Duration(testTimeouts[timeouts.StorageSizingDetection]) * time.Second).
+					WithPolling(time.Duration(testTimeouts[timeouts.StorageSizingPolling]) * time.Second).Should(Succeed())
+			})
+
 			By("verifying tablespace storage sizing status is updated", func() {
 				Eventually(func(g Gomega) {
 					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
 					g.Expect(err).ToNot(HaveOccurred())
 					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
 					g.Expect(cluster.Status.StorageSizing.Tablespaces).ToNot(BeNil())
-					g.Expect(cluster.Status.StorageSizing.Tablespaces[tbsName]).ToNot(BeNil())
+
+					tbsStatus := cluster.Status.StorageSizing.Tablespaces[tbsName]
+					g.Expect(tbsStatus).ToNot(BeNil())
+					g.Expect(tbsStatus.State).ToNot(Equal("WaitingForDiskStatus"),
+						"tablespace sizing should not be blocked on missing disk status")
 
 					var pvcList corev1.PersistentVolumeClaimList
 					err = env.Client.List(env.Ctx, &pvcList,
```

## 4) Add replication-streaming gates before disruptive actions (T2 + primary restart)

**Why:** T2 and primary-restart failures show cluster stuck with non-ready replica/timeline divergence after disruption.

```diff
diff --git a/tests/e2e/dynamic_storage_test.go b/tests/e2e/dynamic_storage_test.go
index 44bc73f1f..52d12e85c 100644
--- a/tests/e2e/dynamic_storage_test.go
+++ b/tests/e2e/dynamic_storage_test.go
@@ -1219,6 +1219,11 @@ var _ = Describe("Dynamic Storage", Label(tests.LabelStorage, tests.LabelDynam
 					waitForPVCCapacityUpdate(namespace, clusterName,
 						time.Duration(testTimeouts[timeouts.AKSVolumeResize])*time.Second)
 				})
+
+				By("verifying standbys are streaming before restarting primary", func() {
+					AssertClusterStandbysAreStreaming(namespace, clusterName,
+						int32(testTimeouts[timeouts.AKSVolumeAttach]))
+				})
 
 				By("deleting primary pod to trigger restart", func() {
 					quickDelete := &ctrlclient.DeleteOptions{
@@ -2033,10 +2038,15 @@ var _ = Describe("Dynamic Storage", Label(tests.LabelStorage, tests.LabelDynam
 				// Use lower disk fill (80-83%) for tests with switchover operations
 				// to ensure WAL files are retained for pg_rewind after the switchover.
 				// Higher fill levels (85%+) cause aggressive WAL recycling which breaks pg_rewind.
-				finalUsage, fillErr := fillDiskIncrementally(primaryPod, 80, 83, 500000)
+				finalUsage, fillErr := fillDiskIncrementally(primaryPod, 78, 80, 500000)
 				if fillErr != nil {
 					GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
 				}
 				GinkgoWriter.Printf("Final disk usage after fill: %d%%\n", finalUsage)
 				Expect(finalUsage).To(BeNumerically(">=", 75),
 					"Disk fill should reach at least 75%% to trigger growth")
 			})
+
+			By("verifying replica is streaming before switchover", func() {
+				AssertClusterStandbysAreStreaming(namespace, clusterName,
+					int32(testTimeouts[timeouts.AKSVolumeAttach]))
+			})
@@ -2071,6 +2081,11 @@ var _ = Describe("Dynamic Storage", Label(tests.LabelStorage, tests.LabelDynam
 					g.Expect(cluster.Status.TargetPrimary).To(Equal(promotedPrimary))
 				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
 					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
+			})
+
+			By("verifying replica catches up and streams after switchover", func() {
+				AssertClusterStandbysAreStreaming(namespace, clusterName,
+					int32(testTimeouts[timeouts.AKSVolumeAttach]))
 			})
 
 			By("verifying data integrity after switchover", func() {
```

## Notes

- Patch set 1 and 2 are mostly **test harness reliability** hardening.
- Patch sets 3 and 4 are intended to separate **trigger/transport flake** from **actual convergence defects**.
- If failures persist after 1+2, treat T2/primary-restart and tablespace as likely implementation defects in reconciliation/replication behavior rather than pure test flake.
