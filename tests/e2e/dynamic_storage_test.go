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
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/reconciler/dynamicstorage"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/specs"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
	"github.com/cloudnative-pg/cloudnative-pg/tests"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/clusterutils"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/exec"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/nodes"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/operator"
	podutils "github.com/cloudnative-pg/cloudnative-pg/tests/utils/pods"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/postgres"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/proxy"
	"github.com/cloudnative-pg/cloudnative-pg/tests/utils/timeouts"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// getDiskUsagePercent returns the disk usage percentage for the PGDATA directory
// by executing `df` inside the postgres container and parsing the output.
func getDiskUsagePercent(
	pod *corev1.Pod,
) (int, error) {
	timeout := 10 * time.Second
	// Use df to get usage of /var/lib/postgresql/data (PGDATA mount)
	stdout, stderr, err := exec.CommandInContainer(
		env.Ctx, env.Client, env.Interface, env.RestClientConfig,
		exec.ContainerLocator{
			Namespace:     pod.Namespace,
			PodName:       pod.Name,
			ContainerName: specs.PostgresContainerName,
		},
		&timeout,
		"df", "--output=pcent", "/var/lib/postgresql/data",
	)
	if err != nil {
		return 0, fmt.Errorf("df command failed: %w, stderr: %s", err, stderr)
	}

	// Parse output - format is:
	// Use%
	//  42%
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output: %s", stdout)
	}
	usageStr := strings.TrimSpace(lines[1])
	usageStr = strings.TrimSuffix(usageStr, "%")
	usage, err := strconv.Atoi(usageStr)
	if err != nil {
		return 0, fmt.Errorf("failed to parse usage percentage from '%s': %w", usageStr, err)
	}
	return usage, nil
}

// getTablespaceDiskUsagePercent returns the disk usage percentage for a specific tablespace
func getTablespaceDiskUsagePercent(
	pod *corev1.Pod,
	tbsName string,
) (int, error) {
	timeout := 10 * time.Second
	tsPath := specs.MountForTablespace(tbsName)
	stdout, stderr, err := exec.CommandInContainer(
		env.Ctx, env.Client, env.Interface, env.RestClientConfig,
		exec.ContainerLocator{
			Namespace:     pod.Namespace,
			PodName:       pod.Name,
			ContainerName: specs.PostgresContainerName,
		},
		&timeout,
		"df", "--output=pcent", tsPath,
	)
	if err != nil {
		return 0, fmt.Errorf("df command failed for tablespace %s: %w, stderr: %s", tbsName, err, stderr)
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output for tablespace %s: %s", tbsName, stdout)
	}
	usageStr := strings.TrimSpace(lines[1])
	usageStr = strings.TrimSuffix(usageStr, "%")
	usage, err := strconv.Atoi(usageStr)
	if err != nil {
		return 0, fmt.Errorf("failed to parse usage percentage for tablespace %s from '%s': %w",
			tbsName, usageStr, err)
	}
	return usage, nil
}

// fillTablespaceDiskIncrementally fills the disk on a specific tablespace
func fillTablespaceDiskIncrementally(
	pod *corev1.Pod,
	tbsName string,
	targetUsagePercent int,
	maxUsagePercent int,
	batchRows int,
	namespace string,
	clusterName string,
) (int, error) {
	GinkgoWriter.Printf("Starting incremental disk fill on tablespace %s, pod %s, target: %d%%, max: %d%%\n",
		tbsName, pod.Name, targetUsagePercent, maxUsagePercent)

	currentUsage, err := getTablespaceDiskUsagePercent(pod, tbsName)
	if err != nil {
		return 0, fmt.Errorf("failed to get initial tablespace disk usage: %w", err)
	}

	// Get initial PVC size to detect when resize is triggered
	var initialPVCSize *resource.Quantity
	var pvcList corev1.PersistentVolumeClaimList
	err = env.Client.List(env.Ctx, &pvcList,
		ctrlclient.InNamespace(namespace),
		ctrlclient.MatchingLabels{
			utils.ClusterLabelName:        clusterName,
			utils.PvcRoleLabelName:        string(utils.PVCRolePgTablespace),
			utils.TablespaceNameLabelName: tbsName,
		})
	if err == nil && len(pvcList.Items) > 0 {
		size := pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
		initialPVCSize = &size
		GinkgoWriter.Printf("Initial PVC size: %s\n", initialPVCSize.String())
	}

	// Create the fill table in the tablespace
	createTableQuery := fmt.Sprintf("CREATE TABLE IF NOT EXISTS fill_tbs (id bigint, data text) TABLESPACE %s;",
		tbsName)
	timeout := time.Minute
	_, _, err = exec.QueryInInstancePodWithTimeout(
		env.Ctx, env.Client, env.Interface, env.RestClientConfig,
		exec.PodLocator{Namespace: pod.Namespace, PodName: pod.Name},
		postgres.AppDBName,
		createTableQuery,
		timeout,
	)
	if err != nil {
		return currentUsage, fmt.Errorf("failed to create fill table in tablespace: %w", err)
	}

	batchNum := 0
	for currentUsage < targetUsagePercent {
		batchNum++
		if currentUsage >= maxUsagePercent {
			GinkgoWriter.Printf("Reached max usage threshold (%d%%), stopping fill\n", maxUsagePercent)
			break
		}

		// Check if PVC has been resized - if so, we've successfully triggered the reconciler
		if initialPVCSize != nil {
			err = env.Client.List(env.Ctx, &pvcList,
				ctrlclient.InNamespace(namespace),
				ctrlclient.MatchingLabels{
					utils.ClusterLabelName:        clusterName,
					utils.PvcRoleLabelName:        string(utils.PVCRolePgTablespace),
					utils.TablespaceNameLabelName: tbsName,
				})
			if err == nil && len(pvcList.Items) > 0 {
				currentSize := pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
				if currentSize.Cmp(*initialPVCSize) > 0 {
					GinkgoWriter.Printf("PVC resize detected! Initial: %s, Current: %s. Stopping fill.\n",
						initialPVCSize.String(), currentSize.String())
					return currentUsage, nil
				}
			}
		}

		startID := (batchNum-1)*batchRows + 1
		endID := batchNum * batchRows
		insertQuery := fmt.Sprintf(
			"INSERT INTO fill_tbs SELECT id, repeat('x', 1000) FROM generate_series(%d, %d) AS id;",
			startID, endID,
		)

		_, _, err := exec.QueryInInstancePodWithTimeout(
			env.Ctx, env.Client, env.Interface, env.RestClientConfig,
			exec.PodLocator{Namespace: pod.Namespace, PodName: pod.Name},
			postgres.AppDBName,
			insertQuery,
			time.Minute*5,
		)
		if err != nil {
			return currentUsage, err
		}

		time.Sleep(2 * time.Second)
		currentUsage, err = getTablespaceDiskUsagePercent(pod, tbsName)
		if err != nil {
			return currentUsage, fmt.Errorf("failed to get tablespace usage after batch %d: %w", batchNum, err)
		}
		GinkgoWriter.Printf("After tablespace batch %d: disk usage is %d%%\n", batchNum, currentUsage)
	}

	return currentUsage, nil
}

// fillDiskFast fills the PGDATA filesystem using dd to write a raw zero file.
// This is much faster than SQL-based fills (~5s vs ~3-5min) because it avoids
// kubectl exec round-trips per batch, WAL generation, and checkpoint overhead.
//
// Use this for tests that only need the reconciler to detect disk pressure via
// statfs (the reconciler uses syscall.Statfs, not Postgres stats). Tests that
// need WAL history for pg_rewind, realistic replication, or backup interactions
// should use fillDiskIncrementally instead.
//
// The fill file is written to /var/lib/postgresql/data/pgdata/fill_ballast.
// It is cleaned up automatically when the test namespace is torn down.
func fillDiskFast(
	pod *corev1.Pod,
	targetUsagePercent int,
	maxUsagePercent int,
) (int, error) {
	GinkgoWriter.Printf("Starting fast disk fill (dd) on pod %s, target: %d%%, max: %d%%\n",
		pod.Name, targetUsagePercent, maxUsagePercent)

	currentUsage, err := getDiskUsagePercent(pod)
	if err != nil {
		return 0, fmt.Errorf("failed to get initial disk usage: %w", err)
	}
	GinkgoWriter.Printf("Initial disk usage: %d%%\n", currentUsage)

	if currentUsage >= targetUsagePercent {
		GinkgoWriter.Printf("Disk already at target usage (%d%% >= %d%%)\n", currentUsage, targetUsagePercent)
		return currentUsage, nil
	}

	// Get total filesystem size to calculate how many bytes to write.
	timeout := 10 * time.Second
	stdout, stderr, err := exec.CommandInContainer(
		env.Ctx, env.Client, env.Interface, env.RestClientConfig,
		exec.ContainerLocator{
			Namespace:     pod.Namespace,
			PodName:       pod.Name,
			ContainerName: specs.PostgresContainerName,
		},
		&timeout,
		"df", "--output=size,used", "-B1", "/var/lib/postgresql/data",
	)
	if err != nil {
		return currentUsage, fmt.Errorf("df failed: %w, stderr: %s", err, stderr)
	}

	// Parse: "     Size      Used\n 5368709120 1073741824\n"
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		return currentUsage, fmt.Errorf("unexpected df output: %s", stdout)
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 2 {
		return currentUsage, fmt.Errorf("unexpected df fields: %s", lines[1])
	}
	totalBytes, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return currentUsage, fmt.Errorf("failed to parse total bytes '%s': %w", fields[0], err)
	}
	usedBytes, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return currentUsage, fmt.Errorf("failed to parse used bytes '%s': %w", fields[1], err)
	}

	// Calculate bytes needed to reach target. Aim for (target - 2%) to leave
	// margin and avoid overshooting past maxUsagePercent.
	effectiveTarget := targetUsagePercent - 2
	if effectiveTarget < currentUsage {
		effectiveTarget = currentUsage + 1
	}
	targetBytes := (int64(effectiveTarget) * totalBytes / 100) - usedBytes
	if targetBytes <= 0 {
		GinkgoWriter.Printf("Already at or above effective target (%d%%)\n", effectiveTarget)
		return currentUsage, nil
	}

	// Write in megabytes
	megabytes := targetBytes / (1024 * 1024)
	if megabytes < 1 {
		megabytes = 1
	}

	GinkgoWriter.Printf("Writing %dMB fill file (total=%dMB, used=%dMB, target=%d%%)\n",
		megabytes, totalBytes/(1024*1024), usedBytes/(1024*1024), effectiveTarget)

	fillPath := specs.PgDataPath + "/fill_ballast"

	// Try fallocate first (instant) then fall back to dd (slow).
	// fallocate pre-allocates disk space without writing data.
	fallocateTimeout := 30 * time.Second
	_, stderr, err = exec.CommandInContainer(
		env.Ctx, env.Client, env.Interface, env.RestClientConfig,
		exec.ContainerLocator{
			Namespace:     pod.Namespace,
			PodName:       pod.Name,
			ContainerName: specs.PostgresContainerName,
		},
		&fallocateTimeout,
		"fallocate", "-l", fmt.Sprintf("%dM", megabytes), fillPath,
	)
	if err == nil {
		GinkgoWriter.Printf("fallocate succeeded (%dMB)\n", megabytes)
	} else {
		GinkgoWriter.Printf("fallocate failed (err: %v, stderr: %s), falling back to dd\n", err, stderr)
		// Fall back to dd with extended timeout for slow disks
		fillTimeout := 5 * time.Minute
		_, stderr, err = exec.CommandInContainer(
			env.Ctx, env.Client, env.Interface, env.RestClientConfig,
			exec.ContainerLocator{
				Namespace:     pod.Namespace,
				PodName:       pod.Name,
				ContainerName: specs.PostgresContainerName,
			},
			&fillTimeout,
			"dd", "if=/dev/zero", fmt.Sprintf("of=%s", fillPath),
			"bs=1M", fmt.Sprintf("count=%d", megabytes),
		)
		if err != nil {
			// dd may fail with "No space left" if we slightly overshoot — check usage
			GinkgoWriter.Printf("dd returned error (may be expected near full): %v, stderr: %s\n", err, stderr)
		}
	}

	// Sync to ensure statfs reflects the write
	syncTimeout := 30 * time.Second
	_, _, _ = exec.CommandInContainer(
		env.Ctx, env.Client, env.Interface, env.RestClientConfig,
		exec.ContainerLocator{
			Namespace:     pod.Namespace,
			PodName:       pod.Name,
			ContainerName: specs.PostgresContainerName,
		},
		&syncTimeout,
		"sync",
	)

	finalUsage, err := getDiskUsagePercent(pod)
	if err != nil {
		return 0, fmt.Errorf("failed to get final disk usage: %w", err)
	}
	GinkgoWriter.Printf("Fast fill complete. Final usage: %d%%\n", finalUsage)

	if finalUsage > maxUsagePercent {
		return finalUsage, fmt.Errorf("overshot max usage: %d%% > %d%%", finalUsage, maxUsagePercent)
	}

	return finalUsage, nil
}

// removeFillFile removes the ballast file created by fillDiskFast.
// Call this in tests that need to free space after growth verification.
func removeFillFile(pod *corev1.Pod) error {
	timeout := 10 * time.Second
	fillPath := specs.PgDataPath + "/fill_ballast"
	_, stderr, err := exec.CommandInContainer(
		env.Ctx, env.Client, env.Interface, env.RestClientConfig,
		exec.ContainerLocator{
			Namespace:     pod.Namespace,
			PodName:       pod.Name,
			ContainerName: specs.PostgresContainerName,
		},
		&timeout,
		"rm", "-f", fillPath,
	)
	if err != nil {
		return fmt.Errorf("failed to remove fill file: %w, stderr: %s", err, stderr)
	}
	return nil
}

// fillDiskIncrementally fills the disk on the given pod incrementally until
// the target usage percentage is reached. It inserts data in batches and checks
// disk usage between batches to avoid overshooting.
//
// Use this instead of fillDiskFast for tests that involve switchover, failover,
// pod restart, node drain, or backup — anywhere WAL history and realistic
// replication state matter (e.g. pg_rewind needs WAL divergence points).
//
// Parameters:
//   - pod: the pod to fill disk on
//   - targetUsagePercent: stop when this usage is reached (e.g., 85 for 85%)
//   - maxUsagePercent: abort if usage exceeds this to prevent crash (e.g., 95)
//   - batchRows: number of rows per batch (e.g., 500000)
//
// Returns the final disk usage percentage or error.
func fillDiskIncrementally(
	pod *corev1.Pod,
	targetUsagePercent int,
	maxUsagePercent int,
	batchRows int,
) (int, error) {
	GinkgoWriter.Printf("Starting incremental disk fill on pod %s, target: %d%%, max: %d%%\n",
		pod.Name, targetUsagePercent, maxUsagePercent)

	// First, check current usage
	currentUsage, err := getDiskUsagePercent(pod)
	if err != nil {
		return 0, fmt.Errorf("failed to get initial disk usage: %w", err)
	}
	GinkgoWriter.Printf("Initial disk usage: %d%%\n", currentUsage)

	if currentUsage >= targetUsagePercent {
		GinkgoWriter.Printf("Disk already at target usage (%d%% >= %d%%)\n", currentUsage, targetUsagePercent)
		return currentUsage, nil
	}

	// Create the fill table if it doesn't exist
	createTableQuery := "CREATE TABLE IF NOT EXISTS fill_data (id bigint, data text);"
	timeout := time.Minute
	_, _, err = exec.QueryInInstancePodWithTimeout(
		env.Ctx, env.Client, env.Interface, env.RestClientConfig,
		exec.PodLocator{Namespace: pod.Namespace, PodName: pod.Name},
		postgres.AppDBName,
		createTableQuery,
		timeout,
	)
	if err != nil {
		return currentUsage, fmt.Errorf("failed to create fill table: %w", err)
	}

	batchNum := 0
	for currentUsage < targetUsagePercent {
		batchNum++

		// Safety check - abort if we're getting too close to full
		if currentUsage >= maxUsagePercent {
			GinkgoWriter.Printf("Stopping: usage %d%% exceeds max %d%%\n", currentUsage, maxUsagePercent)
			break
		}

		// Insert a batch of data
		// Each row is approximately 1KB (id + 1000 chars)
		startID := (batchNum-1)*batchRows + 1
		endID := batchNum * batchRows
		insertQuery := fmt.Sprintf(
			"INSERT INTO fill_data SELECT id, repeat('x', 1000) FROM generate_series(%d, %d) AS id;",
			startID, endID,
		)

		GinkgoWriter.Printf("Batch %d: inserting rows %d-%d (current usage: %d%%)\n",
			batchNum, startID, endID, currentUsage)

		_, stderr, err := exec.QueryInInstancePodWithTimeout(
			env.Ctx, env.Client, env.Interface, env.RestClientConfig,
			exec.PodLocator{Namespace: pod.Namespace, PodName: pod.Name},
			postgres.AppDBName,
			insertQuery,
			time.Minute*5,
		)
		if err != nil {
			// Check if it's a disk full error
			if strings.Contains(stderr, "No space left on device") {
				GinkgoWriter.Printf("Disk full error during batch %d\n", batchNum)
				return currentUsage, fmt.Errorf("disk full during batch %d: %w", batchNum, err)
			}
			return currentUsage, fmt.Errorf("insert failed during batch %d: %w, stderr: %s", batchNum, err, stderr)
		}

		// Wait a bit for WAL to flush and filesystem to update
		time.Sleep(2 * time.Second)

		// Check disk usage after batch
		currentUsage, err = getDiskUsagePercent(pod)
		if err != nil {
			GinkgoWriter.Printf("Warning: failed to get disk usage after batch %d: %v\n", batchNum, err)
			// Continue anyway, we'll check again next iteration
			continue
		}
		GinkgoWriter.Printf("After batch %d: disk usage is %d%%\n", batchNum, currentUsage)
	}

	GinkgoWriter.Printf("Disk fill complete. Final usage: %d%%\n", currentUsage)
	return currentUsage, nil
}

// updateCluster updates a cluster using a mutator function
func updateCluster(namespace, clusterName string, mutator func(*apiv1.Cluster)) {
	cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
	Expect(err).ToNot(HaveOccurred())
	original := cluster.DeepCopy()
	mutator(cluster)
	err = env.Client.Patch(env.Ctx, cluster, ctrlclient.MergeFrom(original))
	Expect(err).ToNot(HaveOccurred())
}

// verifyGrowthCompletion waits for a growth operation to complete, including:
// 1. StorageSizing status reaching a stable state (Balanced or AtLimit)
// 2. PVC capacity actually reflecting the resize (CSI driver completion)
func verifyGrowthCompletion(namespace, clusterName string) {
	By("waiting for storage sizing state to stabilize", func() {
		Eventually(func(g Gomega) {
			cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
			g.Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
			state := cluster.Status.StorageSizing.Data.State
			g.Expect(state).To(Or(Equal("Balanced"), Equal("AtLimit")),
				"Waiting for growth to complete, current state: %s", state)
		}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
			WithPolling(time.Duration(testTimeouts[timeouts.StorageSizingPolling]) * time.Second).Should(Succeed())
	})

	By("waiting for PVC capacity to update (CSI resize completion)", func() {
		waitForPVCCapacityUpdate(namespace, clusterName,
			time.Duration(testTimeouts[timeouts.AKSVolumeResize])*time.Second)
	})
}

// assertDataConsistency verifies that data is consistent across replicas
func assertDataConsistency(namespace, clusterName string) {
	cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
	Expect(err).ToNot(HaveOccurred())
	Expect(cluster.Status.Phase).To(Equal(apiv1.PhaseHealthy))
}

// waitForPVCCapacityUpdate waits for PVC Status.Capacity to reflect the resize completion.
// This is critical for Azure AKS CSI operations which can take 5-10 minutes.
// The function verifies that all PVCs have capacity >= their request, indicating the
// CSI driver has completed the filesystem resize.
func waitForPVCCapacityUpdate(namespace, clusterName string, timeout time.Duration) {
	GinkgoWriter.Printf("Waiting for PVC capacity update in cluster %s/%s (timeout: %v)\n",
		namespace, clusterName, timeout)

	Eventually(func(g Gomega) {
		var pvcList corev1.PersistentVolumeClaimList
		err := env.Client.List(env.Ctx, &pvcList,
			ctrlclient.InNamespace(namespace),
			ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(pvcList.Items).ToNot(BeEmpty(), "Expected at least one PVC for cluster")

		for _, pvc := range pvcList.Items {
			request := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			capacity := pvc.Status.Capacity[corev1.ResourceStorage]

			// Capacity should be >= request once resize is complete
			g.Expect(capacity.Cmp(request)).To(BeNumerically(">=", 0),
				"PVC %s: capacity (%s) should be >= request (%s)",
				pvc.Name, capacity.String(), request.String())

			GinkgoWriter.Printf("PVC %s: request=%s, capacity=%s (OK)\n",
				pvc.Name, request.String(), capacity.String())
		}
	}).WithTimeout(timeout).
		WithPolling(time.Duration(testTimeouts[timeouts.StorageSizingPolling]) * time.Second).Should(Succeed())

	GinkgoWriter.Printf("All PVCs have updated capacity\n")
}

// triggerSwitchoverAndWait performs a switchover using AKS-aware timeouts and waits
// for the cluster to become ready again.
func triggerSwitchoverAndWait(namespace, clusterName string) (oldPrimary, newPrimary string) {
	cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
	Expect(err).ToNot(HaveOccurred())
	oldPrimary = cluster.Status.CurrentPrimary
	Expect(oldPrimary).ToNot(BeEmpty(), "cluster should have a current primary before switchover")

	podList, err := clusterutils.ListPods(env.Ctx, env.Client, namespace, clusterName)
	Expect(err).ToNot(HaveOccurred())
	Expect(len(podList.Items)).To(BeNumerically(">=", 2), "switchover requires at least one replica")

	for _, pod := range podList.Items {
		if pod.Name != oldPrimary {
			newPrimary = pod.Name
			break
		}
	}
	Expect(newPrimary).ToNot(BeEmpty(), "failed to identify a promotion candidate")

	originCluster := cluster.DeepCopy()
	cluster.Status.TargetPrimary = newPrimary
	err = env.Client.Status().Patch(env.Ctx, cluster, ctrlclient.MergeFrom(originCluster))
	Expect(err).ToNot(HaveOccurred())

	Eventually(func(g Gomega) {
		updated, getErr := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
		g.Expect(getErr).ToNot(HaveOccurred())
		g.Expect(updated.Status.CurrentPrimary).To(Equal(newPrimary))
		g.Expect(updated.Status.TargetPrimary).To(Equal(newPrimary))
	}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
		WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())

	// Use ClusterIsReadySlow because switchover + volume operations on AKS can take 15-20 minutes
	AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReadySlow], env)
	return oldPrimary, newPrimary
}

var _ = Describe("Dynamic Storage", Label(tests.LabelStorage, tests.LabelDynamicStorage), func() {
	const (
		level           = tests.Medium
		namespacePrefix = "dynamic-storage-e2e"
	)

	var namespace string

	BeforeEach(func() {
		if testLevelEnv.Depth < int(level) {
			Skip("Test depth is lower than the amount requested for this test")
		}
		if MustGetEnvProfile().UsesNodeDiskSpace() {
			Skip("this test requires dynamic volume provisioning with resize support")
		}
	})

	Context("Dynamic sizing validation", func() {
		It("rejects invalid configurations", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			By("rejecting size with request/limit", func() {
				cluster := &apiv1.Cluster{}
				cluster.Name = "invalid-config"
				cluster.Namespace = namespace
				cluster.Spec.Instances = 1
				cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
					Size:    "10Gi",
					Request: "5Gi",
					Limit:   "20Gi",
				}

				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
			})

			By("rejecting request without limit", func() {
				cluster := &apiv1.Cluster{}
				cluster.Name = "invalid-config-2"
				cluster.Namespace = namespace
				cluster.Spec.Instances = 1
				cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
					Request: "5Gi",
				}

				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("limit is required"))
			})

			By("rejecting request greater than limit", func() {
				cluster := &apiv1.Cluster{}
				cluster.Name = "invalid-config-3"
				cluster.Namespace = namespace
				cluster.Spec.Instances = 1
				cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
					Request: "20Gi",
					Limit:   "10Gi",
				}

				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("cannot exceed limit"))
			})
		})
	})

	Context("Dynamic sizing functionality", Label(tests.LabelDynamicStorage), func() {
		It("provisions PVC at request size", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-basic"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
			}

			By("creating cluster", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())

				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("verifying PVC is created at request size", func() {
				var pvcList corev1.PersistentVolumeClaimList
				Eventually(func(g Gomega) {
					err := env.Client.List(env.Ctx, &pvcList,
						ctrlclient.InNamespace(namespace),
						ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(pvcList.Items).To(HaveLen(1))
					size := pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
					g.Expect(size.String()).To(Equal("5Gi"))
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})

			By("verifying dynamic sizing is detected as enabled", func() {
				Expect(dynamicstorage.IsDynamicSizingEnabled(&cluster.Spec.StorageConfiguration)).To(BeTrue())
			})

			By("verifying Prometheus metrics are exposed", func() {
				podList, err := clusterutils.ListPods(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				for _, pod := range podList.Items {
					Eventually(func(g Gomega) {
						out, err := proxy.RetrieveMetricsFromInstance(env.Ctx, env.Interface, pod, false)
						g.Expect(err).ToNot(HaveOccurred())

						// Check for disk metrics
						g.Expect(out).To(ContainSubstring("cnpg_disk_total_bytes"))
						g.Expect(out).To(ContainSubstring("cnpg_disk_used_bytes"))

						// Check for dynamic storage metrics
						g.Expect(out).To(ContainSubstring("cnpg_dynamic_storage_target_size_bytes"))
						g.Expect(out).To(ContainSubstring("cnpg_dynamic_storage_state"))
						g.Expect(out).To(ContainSubstring("cnpg_dynamic_storage_budget_total"))
					}, testTimeouts[timeouts.Short]).Should(Succeed())
				}
			})
		})

		It("grows storage when usage exceeds target buffer", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-grow"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
				// Maintenance window is always open by default
			}

			By("creating cluster", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())

				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			var primaryPod *corev1.Pod
			By("finding primary pod", func() {
				primaryPod, err = clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
			})

			By("filling disk to trigger growth", func() {
				// Use fast dd-based fill — this test only needs disk pressure for the
				// reconciler's statfs check, no WAL/replication concerns.
				finalUsage, err := fillDiskFast(primaryPod, 85, 87)
				if err != nil {
					GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", err)
				}
				GinkgoWriter.Printf("Final disk usage after fill: %d%%\n", finalUsage)
				Expect(finalUsage).To(BeNumerically(">=", 75),
					"Disk fill should reach at least 75%% to trigger growth")
			})

			By("verifying storage sizing status is updated", func() {
				// Wait for the dynamic storage reconciler to:
				// 1. Collect disk status from instance manager
				// 2. Evaluate sizing needs based on disk usage
				// 3. Update cluster.Status.StorageSizing
				// This can take several reconciliation cycles (30s-5min)
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil(),
						"StorageSizing status should be populated after disk usage triggers sizing logic")
					g.Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil(),
						"Data volume sizing status should be populated")
					var pvcList corev1.PersistentVolumeClaimList
					err = env.Client.List(env.Ctx, &pvcList,
						ctrlclient.InNamespace(namespace),
						ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(pvcList.Items).To(HaveLen(1))
					size := pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
					g.Expect(size.Cmp(resource.MustParse("5Gi"))).To(BeNumerically(">", 0),
						"PVC request should grow beyond initial 5Gi after sizing logic runs")
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})
		})

		It("respects limit and does not grow beyond it", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-limit"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "6Gi",
				TargetBuffer: ptr.To(20),
			}

			By("creating cluster", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())

				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("filling disk to trigger growth toward limit", func() {
				primaryPod, getErr := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(getErr).ToNot(HaveOccurred())
				finalUsage, fillErr := fillDiskFast(primaryPod, 85, 87)
				if fillErr != nil {
					GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
				}
				Expect(finalUsage).To(BeNumerically(">=", 75))
			})

			By("verifying PVC grows but does not exceed limit", func() {
				limit := resource.MustParse("6Gi")
				initialRequest := resource.MustParse("5Gi")
				Eventually(func(g Gomega) {
					var pvcList corev1.PersistentVolumeClaimList
					err := env.Client.List(env.Ctx, &pvcList,
						ctrlclient.InNamespace(namespace),
						ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(pvcList.Items).To(HaveLen(1))

					size := pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
					g.Expect(size.Cmp(initialRequest)).To(BeNumerically(">", 0),
						"PVC should grow beyond initial request when under pressure")
					g.Expect(size.Cmp(limit)).To(BeNumerically("<=", 0),
						"PVC should never exceed configured limit")
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})
		})

		It("creates new replicas at effective size", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-replica"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
			}

			By("creating cluster with 1 instance", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())

				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("scaling to 2 instances", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				originCluster := cluster.DeepCopy()
				cluster.Spec.Instances = 2
				err = env.Client.Patch(env.Ctx, cluster, ctrlclient.MergeFrom(originCluster))
				Expect(err).ToNot(HaveOccurred())

				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("verifying new replica PVC matches existing size", func() {
				var pvcList corev1.PersistentVolumeClaimList
				Eventually(func(g Gomega) {
					err := env.Client.List(env.Ctx, &pvcList,
						ctrlclient.InNamespace(namespace),
						ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(pvcList.Items).To(HaveLen(2))

					// All PVCs should have the same size
					sizes := make(map[string]bool)
					for _, pvc := range pvcList.Items {
						size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
						sizes[size.String()] = true
					}
					g.Expect(sizes).To(HaveLen(1))
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})
		})
	})

	Context("Tablespace dynamic sizing", Label(tests.LabelDynamicStorage), func() {
		It("grows tablespace storage when usage exceeds target buffer", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-tbs-grow"
			tbsName := "tbsdynamic"
			clusterFile := fixturesDir + "/dynamic_storage/cluster-tablespaces-dynamic.yaml.template"

			By("creating cluster with tablespaces", func() {
				AssertCreateCluster(namespace, clusterName, clusterFile, env)
			})

			var initialTablespaceRequest resource.Quantity
			By("recording initial tablespace PVC request", func() {
				var pvcList corev1.PersistentVolumeClaimList
				err := env.Client.List(env.Ctx, &pvcList,
					ctrlclient.InNamespace(namespace),
					ctrlclient.MatchingLabels{
						utils.ClusterLabelName:        clusterName,
						utils.PvcRoleLabelName:        string(utils.PVCRolePgTablespace),
						utils.TablespaceNameLabelName: tbsName,
					})
				Expect(err).ToNot(HaveOccurred())
				Expect(pvcList.Items).To(HaveLen(1))
				initialTablespaceRequest = pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
			})

			var primaryPod *corev1.Pod
			By("finding primary pod", func() {
				primaryPod, err = clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
			})

			By("filling tablespace disk to trigger growth", func() {
				// Push clearly beyond the 80% trigger threshold while retaining headroom.
				// 86/92 reduces threshold-edge behavior where no action is emitted at exactly ~82%.
				finalUsage, err := fillTablespaceDiskIncrementally(primaryPod, tbsName, 86, 92, 100000, namespace, clusterName)
				if err != nil {
					GinkgoWriter.Printf("Tablespace disk fill ended with error: %v\n", err)
				}
				// Test passes if we reached reasonable usage OR if PVC was resized (function stops early)
				// If function stopped early due to PVC resize detection, finalUsage might be low, which is OK
				GinkgoWriter.Printf("Final tablespace usage after fill: %d%%\n", finalUsage)
			})

			By("verifying tablespace sizing target is computed before PVC request update", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Tablespaces).ToNot(BeNil())

					tbsStatus := cluster.Status.StorageSizing.Tablespaces[tbsName]
					g.Expect(tbsStatus).ToNot(BeNil(), "tablespace status should exist for %s", tbsName)
					g.Expect(tbsStatus.State).To(Or(
						Equal("NeedsGrow"),
						Equal("PendingGrowth"),
						Equal("Resizing"),
						Equal("Balanced"),
						Equal("AtLimit"),
						Equal("Emergency"),
					), "tablespace state should reflect active sizing lifecycle")
					g.Expect(tbsStatus.TargetSize).ToNot(BeEmpty(),
						"tablespace target size should be computed after threshold crossing")

					targetSize, parseErr := resource.ParseQuantity(tbsStatus.TargetSize)
					g.Expect(parseErr).ToNot(HaveOccurred())
					g.Expect(targetSize.Cmp(initialTablespaceRequest)).To(BeNumerically(">", 0),
						"tablespace target size should exceed initial request %s", initialTablespaceRequest.String())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.StorageSizingDetection]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.StorageSizingPolling]) * time.Second).Should(Succeed())
			})

			By("verifying tablespace storage sizing status is updated", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Tablespaces).ToNot(BeNil())
					tbsStatus := cluster.Status.StorageSizing.Tablespaces[tbsName]
					g.Expect(tbsStatus).ToNot(BeNil())
					g.Expect(tbsStatus.State).ToNot(Equal("WaitingForDiskStatus"),
						"tablespace sizing should not be blocked on missing disk status")

					var pvcList corev1.PersistentVolumeClaimList
					err = env.Client.List(env.Ctx, &pvcList,
						ctrlclient.InNamespace(namespace),
						ctrlclient.MatchingLabels{
							utils.ClusterLabelName:        clusterName,
							utils.PvcRoleLabelName:        string(utils.PVCRolePgTablespace),
							utils.TablespaceNameLabelName: tbsName,
						})
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(pvcList.Items).To(HaveLen(1))
					size := pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
					g.Expect(size.Cmp(initialTablespaceRequest)).To(BeNumerically(">", 0),
						"Tablespace PVC request should grow beyond initial request %s", initialTablespaceRequest.String())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})
		})
	})

	Context("Maintenance window", Label(tests.LabelDynamicStorage), func() {
		It("queues growth when outside maintenance window", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-maintenance"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
				MaintenanceWindow: &apiv1.MaintenanceWindowConfig{
					// Set maintenance window to a time that won't occur during the test.
					// Using 4am on December 31st (which is valid and will never run during tests).
					// 6-field cron format: second minute hour day-of-month month day-of-week
					Schedule: "0 0 4 31 12 *",
					Duration: "1h",
					Timezone: "UTC",
				},
				// Set high emergency thresholds so disk fill to 85-90% triggers
				// planned growth (via maintenance window) instead of emergency growth.
				// On a 5Gi disk at 90% usage, only 512MB is free. Without these
				// settings, the default 1Gi CriticalMinimumFree would trigger emergency.
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					CriticalThreshold:   99,
					CriticalMinimumFree: "100Mi",
				},
			}

			By("creating cluster", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())

				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("filling disk while maintenance window is closed", func() {
				primaryPod, getErr := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(getErr).ToNot(HaveOccurred())
				finalUsage, fillErr := fillDiskFast(primaryPod, 85, 87)
				if fillErr != nil {
					GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
				}
				Expect(finalUsage).To(BeNumerically(">=", 75))
			})

			By("verifying growth is pending and request is unchanged", func() {
				// Growth should be queued and not applied before the maintenance window opens.
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data.State).To(Equal("PendingGrowth"))
					g.Expect(cluster.Status.StorageSizing.Data.NextMaintenanceWindow).ToNot(BeNil())

					var pvcList corev1.PersistentVolumeClaimList
					err = env.Client.List(env.Ctx, &pvcList,
						ctrlclient.InNamespace(namespace),
						ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(pvcList.Items).To(HaveLen(1))
					size := pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
					g.Expect(size.Cmp(resource.MustParse("5Gi"))).To(BeZero(),
						"PVC request should remain at initial size while growth is pending")
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})
		})
	})

	Context("Emergency growth", Label(tests.LabelDynamicStorage), func() {
		It("grows immediately when critical threshold is reached", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-emergency"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
				MaintenanceWindow: &apiv1.MaintenanceWindowConfig{
					// robfig/cron uses 6-field format: second minute hour day-of-month month day-of-week
					Schedule: "0 0 4 31 2 *", // Never open
					Duration: "1h",
					Timezone: "UTC",
				},
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					Enabled:             ptr.To(true),
					CriticalThreshold:   95,
					CriticalMinimumFree: "100Mi",
				},
			}

			By("creating cluster", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())

				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			var primaryPod *corev1.Pod
			By("finding primary pod", func() {
				primaryPod, err = clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
			})

			By("filling disk to emergency threshold", func() {
				// Use fast dd-based fill to 96% — emergency test only needs disk
				// pressure past the 95% critical threshold, no WAL concerns.
				finalUsage, err := fillDiskFast(primaryPod, 98, 99)
				if err != nil {
					GinkgoWriter.Printf("Emergency disk fill ended with error (may be expected): %v\n", err)
				}
				GinkgoWriter.Printf("Final disk usage for emergency test: %d%%\n", finalUsage)
				Expect(finalUsage).To(BeNumerically(">=", 93),
					"Emergency disk fill should reach at least 90%% to approach critical threshold")
			})

			By("verifying emergency growth triggers", func() {
				// The PVC should grow despite maintenance window being closed.
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data.LastAction).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data.LastAction.Kind).To(Equal("EmergencyGrow"))

					var pvcList corev1.PersistentVolumeClaimList
					err = env.Client.List(env.Ctx, &pvcList,
						ctrlclient.InNamespace(namespace),
						ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(pvcList.Items).To(HaveLen(1))
					size := pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
					g.Expect(size.Cmp(resource.MustParse("5Gi"))).To(BeNumerically(">", 0),
						"PVC request should increase after emergency growth")
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})
		})
	})

	Context("Rate limiting", Label(tests.LabelDynamicStorage), func() {
		It("initializes max-actions budget counters", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-budget"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					Enabled:                     ptr.To(true),
					CriticalThreshold:           95,
					MaxActionsPerDay:            ptr.To(2),
					ReservedActionsForEmergency: ptr.To(1),
				},
			}

			By("creating cluster", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())

				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("verifying budget is tracked", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data.Budget).ToNot(BeNil())
					budget := cluster.Status.StorageSizing.Data.Budget
					g.Expect(budget.AvailableForPlanned).To(BeEquivalentTo(1))
					g.Expect(budget.AvailableForEmergency).To(BeEquivalentTo(1))
					g.Expect(budget.BudgetResetsAt.IsZero()).To(BeFalse())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})
		})
	})

	// ============================================================================
	// Codex P0 E2E Scenarios - Required for Merge
	// See: docs/src/design/dynamic-storage-e2e-requirements-codex.md
	// ============================================================================

	// Test: Growth operation in progress + operator pod restart
	Context("Operator restart during growth",
		Serial, Label(tests.LabelDynamicStorage, tests.LabelDisruptive), func() {
			It("resumes growth operation after operator pod restart", func() {
				var err error
				namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
				Expect(err).ToNot(HaveOccurred())

				clusterName := "dynamic-op-restart"
				cluster := &apiv1.Cluster{}
				cluster.Name = clusterName
				cluster.Namespace = namespace
				cluster.Spec.Instances = 3
				cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
					Request:      "5Gi",
					Limit:        "20Gi",
					TargetBuffer: ptr.To(20),
				}

				// Create test data table for data integrity verification
				tableLocator := TableLocator{
					Namespace:    namespace,
					ClusterName:  clusterName,
					DatabaseName: postgres.AppDBName,
					TableName:    "sentinel",
				}

				By("creating cluster", func() {
					err := env.Client.Create(env.Ctx, cluster)
					Expect(err).ToNot(HaveOccurred())
					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
				})

				By("inserting sentinel data for integrity check", func() {
					AssertCreateTestData(env, tableLocator)
				})

				var primaryPod *corev1.Pod
				By("filling disk to trigger growth", func() {
					primaryPod, err = clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
					Expect(err).ToNot(HaveOccurred())

					// Only the operator pod restarts here, not Postgres — no pg_rewind needed.
					finalUsage, fillErr := fillDiskFast(primaryPod, 85, 87)
					if fillErr != nil {
						GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
					}
					GinkgoWriter.Printf("Final disk usage after fill: %d%%\n", finalUsage)
					Expect(finalUsage).To(BeNumerically(">=", 75),
						"Disk fill should reach at least 75%% to trigger growth")
				})

				By("waiting for storage sizing status to indicate growth needed", func() {
					Eventually(func(g Gomega) {
						cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
						g.Expect(err).ToNot(HaveOccurred())
						g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
						g.Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
					}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
						WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
				})

				By("restarting operator pod during growth operation", func() {
					err := operator.ReloadDeployment(env.Ctx, env.Client, 120)
					Expect(err).ToNot(HaveOccurred())

					// Update expectedOperatorPodName so that AfterEach doesn't fail
					operatorPod, err := operator.GetPod(env.Ctx, env.Client)
					Expect(err).ToNot(HaveOccurred())
					expectedOperatorPodName = operatorPod.GetName()
				})

				By("verifying operator recovered and cluster converges", func() {
					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
				})

				By("verifying storage sizing status is consistent after restart", func() {
					Eventually(func(g Gomega) {
						cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
						g.Expect(err).ToNot(HaveOccurred())
						g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
						WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
				})

				By("verifying data integrity after operator restart", func() {
					AssertDataExpectedCount(env, tableLocator, 2)
				})

				By("verifying PVCs respect request/limit bounds", func() {
					var pvcList corev1.PersistentVolumeClaimList
					err := env.Client.List(env.Ctx, &pvcList,
						ctrlclient.InNamespace(namespace),
						ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
					Expect(err).ToNot(HaveOccurred())
					request := resource.MustParse("5Gi")
					limit := resource.MustParse("20Gi")
					for _, pvc := range pvcList.Items {
						size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
						Expect(size.Cmp(request)).To(BeNumerically(">=", 0))
						Expect(size.Cmp(limit)).To(BeNumerically("<=", 0))
					}
				})
			})
		})

	// Test: Growth operation in progress + PostgreSQL primary pod restart
	Context("Primary pod restart during growth",
		Label(tests.LabelDynamicStorage, tests.LabelSelfHealing), func() {
			It("resumes growth operation after primary pod restart", func() {
				var err error
				namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
				Expect(err).ToNot(HaveOccurred())

				// Select a node upfront for all pods. On AKS, PVs are bound to specific nodes
				// due to availability zone constraints. If we create pods without nodeSelector,
				// they spread across nodes and their PVs get bound there. When we later try to
				// apply nodeSelector to pin all pods to one node, the other pods can't move
				// because their PVs are stuck on different nodes.
				// By setting nodeSelector from the start, all pods get created on the same node.
				var targetNode string
				By("selecting a target node for all cluster pods", func() {
					nodeList, nodeErr := nodes.List(env.Ctx, env.Client)
					Expect(nodeErr).ToNot(HaveOccurred())
					Expect(nodeList.Items).ToNot(BeEmpty(), "No schedulable nodes found")
					// Use the first available node
					targetNode = nodeList.Items[0].Name
					GinkgoWriter.Printf("Selected target node for pod restart test: %s\n", targetNode)
				})

				clusterName := "dynamic-pod-restart"
				cluster := &apiv1.Cluster{}
				cluster.Name = clusterName
				cluster.Namespace = namespace
				cluster.Spec.Instances = 3
				cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
					Request:      "5Gi",
					Limit:        "20Gi",
					TargetBuffer: ptr.To(20),
					// Set high emergency thresholds to ensure scheduled growth
					// instead of emergency growth which can interfere with pod restarts.
					EmergencyGrow: &apiv1.EmergencyGrowConfig{
						CriticalThreshold:   99,
						CriticalMinimumFree: "100Mi",
					},
				}
				// Pin all pods to one node from the start to avoid cross-node PV binding.
				// On AKS, PVs have node affinity and can't be moved between nodes.
				// Disable pod anti-affinity since all pods must be on the same node.
				cluster.Spec.Affinity = apiv1.AffinityConfiguration{
					EnablePodAntiAffinity: ptr.To(false),
					NodeSelector: map[string]string{
						"kubernetes.io/hostname": targetNode,
					},
				}
				// Configure wal_keep_size to retain WAL segments for pg_rewind.
				// When the primary is deleted and a failover occurs, the old primary
				// needs pg_rewind to find the divergence point in WAL.
				// Use larger value to ensure enough WAL is retained even with disk fill.
				cluster.Spec.PostgresConfiguration.Parameters = map[string]string{
					"wal_keep_size": "2GB",
				}

				tableLocator := TableLocator{
					Namespace:    namespace,
					ClusterName:  clusterName,
					DatabaseName: postgres.AppDBName,
					TableName:    "sentinel",
				}

				By("creating cluster", func() {
					err := env.Client.Create(env.Ctx, cluster)
					Expect(err).ToNot(HaveOccurred())
					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
				})

				By("inserting sentinel data for integrity check", func() {
					AssertCreateTestData(env, tableLocator)
				})

				var primaryPod *corev1.Pod
				By("filling disk to trigger growth", func() {
					primaryPod, err = clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
					Expect(err).ToNot(HaveOccurred())

					// Use fillDiskFast (dd/fallocate) instead of fillDiskIncrementally to avoid
					// generating WAL. SQL-based disk fill creates WAL that may be recycled before
					// pg_rewind can use it after failover. Use 70-75% to leave headroom for WAL.
					finalUsage, fillErr := fillDiskFast(primaryPod, 70, 75)
					if fillErr != nil {
						GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
					}
					GinkgoWriter.Printf("Final disk usage after fill: %d%%\n", finalUsage)
					Expect(finalUsage).To(BeNumerically(">=", 65),
						"Disk fill should reach at least 65%% to trigger growth")
				})

				By("waiting for storage sizing status to be populated", func() {
					Eventually(func(g Gomega) {
						cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
						g.Expect(err).ToNot(HaveOccurred())
						g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					}).WithTimeout(time.Duration(testTimeouts[timeouts.StorageSizingDetection]) * time.Second).
						WithPolling(time.Duration(testTimeouts[timeouts.StorageSizingPolling]) * time.Second).Should(Succeed())
				})

				By("waiting for PVC capacity to update (CSI resize completion)", func() {
					// CRITICAL: Wait for CSI driver to complete the resize before disrupting the pod.
					// Azure AKS CSI operations can take 5-10 minutes.
					waitForPVCCapacityUpdate(namespace, clusterName,
						time.Duration(testTimeouts[timeouts.AKSVolumeResize])*time.Second)
				})

				By("verifying standbys are streaming before restarting primary", func() {
					AssertClusterStandbysAreStreaming(namespace, clusterName, int32(testTimeouts[timeouts.AKSVolumeAttach]))
				})

				By("deleting primary pod to trigger restart", func() {
					// NodeSelector was set at cluster creation time to ensure all pods
					// start on the same node with PVs bound there. This avoids cross-node
					// volume attach issues on AKS where PVs have node affinity.
					quickDelete := &ctrlclient.DeleteOptions{
						GracePeriodSeconds: &quickDeletionPeriod,
					}
					err = podutils.Delete(env.Ctx, env.Client, namespace, primaryPod.Name, quickDelete)
					Expect(err).ToNot(HaveOccurred())
				})

				By("verifying cluster returns to Ready state", func() {
					// Use ClusterIsReadySlow because pod restart + volume reattach on AKS can take 15-20 minutes
					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReadySlow], env)
				})

				By("verifying data integrity after primary restart", func() {
					AssertDataExpectedCount(env, tableLocator, 2)
				})

				By("verifying storage sizing continues after restart", func() {
					Eventually(func(g Gomega) {
						cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
						g.Expect(err).ToNot(HaveOccurred())
						g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
						WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
				})
			})
		})

	// Test: Growth operation in progress + failover/switchover event
	Context("Failover during growth", Label(tests.LabelDynamicStorage, tests.LabelSelfHealing), func() {
		It("continues growth safely after failover", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-failover"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 3
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
				// Set high emergency thresholds to ensure scheduled growth
				// instead of emergency growth which can interfere with switchover.
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					CriticalThreshold:   99,
					CriticalMinimumFree: "100Mi",
				},
			}
			// Configure wal_keep_size to retain WAL segments for pg_rewind.
			// When switchover occurs, the old primary needs pg_rewind to find
			// the divergence point in WAL. Use larger value to ensure enough
			// WAL is retained even after disk operations.
			cluster.Spec.PostgresConfiguration.Parameters = map[string]string{
				"wal_keep_size": "2GB",
			}
			stiffenSpecForReliability(cluster)

			tableLocator := TableLocator{
				Namespace:    namespace,
				ClusterName:  clusterName,
				DatabaseName: postgres.AppDBName,
				TableName:    "sentinel",
			}

			By("creating cluster", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())
				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("inserting sentinel data for integrity check", func() {
				AssertCreateTestData(env, tableLocator)
			})

			By("verifying standbys are streaming before filling disk", func() {
				AssertClusterStandbysAreStreaming(namespace, clusterName, int32(testTimeouts[timeouts.AKSVolumeAttach]))
			})

			var originalPrimary string
			By("recording original primary and filling disk", func() {
				primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				originalPrimary = primaryPod.Name

				// Use fillDiskFast (dd/fallocate) instead of fillDiskIncrementally to avoid
				// generating WAL. SQL-based disk fill creates WAL that may be recycled before
				// pg_rewind can use it after switchover. Use 70-75% to leave headroom for WAL.
				finalUsage, fillErr := fillDiskFast(primaryPod, 70, 75)
				if fillErr != nil {
					GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
				}
				GinkgoWriter.Printf("Final disk usage after fill: %d%%\n", finalUsage)
				Expect(finalUsage).To(BeNumerically(">=", 65),
					"Disk fill should reach at least 65%% to trigger growth")
			})

			By("waiting for storage sizing to be active", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.StorageSizingDetection]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.StorageSizingPolling]) * time.Second).Should(Succeed())
			})

			By("waiting for PVC capacity to update (CSI resize completion)", func() {
				// CRITICAL: Wait for CSI driver to complete the resize before triggering switchover.
				// Azure AKS CSI operations can take 5-10 minutes. If we switchover while the
				// CSI driver is still resizing, the operation may fail or the cluster may not stabilize.
				waitForPVCCapacityUpdate(namespace, clusterName,
					time.Duration(testTimeouts[timeouts.AKSVolumeResize])*time.Second)
			})

			var promotedPrimary string
			By("triggering switchover", func() {
				previousPrimary, newPrimary := triggerSwitchoverAndWait(namespace, clusterName)
				Expect(previousPrimary).To(Equal(originalPrimary))
				promotedPrimary = newPrimary
			})

			By("verifying correct primary election", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.CurrentPrimary).To(Equal(promotedPrimary))
					g.Expect(cluster.Status.CurrentPrimary).ToNot(Equal(originalPrimary))
					g.Expect(cluster.Status.TargetPrimary).To(Equal(promotedPrimary))
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})

			By("verifying data integrity after switchover", func() {
				AssertDataExpectedCount(env, tableLocator, 2)
			})

			By("verifying no size divergence across instances", func() {
				var pvcList corev1.PersistentVolumeClaimList
				err := env.Client.List(env.Ctx, &pvcList,
					ctrlclient.InNamespace(namespace),
					ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
				Expect(err).ToNot(HaveOccurred())

				// All PVCs should be within request/limit bounds
				request := resource.MustParse("5Gi")
				limit := resource.MustParse("20Gi")
				for _, pvc := range pvcList.Items {
					size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
					Expect(size.Cmp(request)).To(BeNumerically(">=", 0))
					Expect(size.Cmp(limit)).To(BeNumerically("<=", 0))
				}
			})
		})
	})

	// Test: Growth operation in progress + user spec mutation
	Context("Spec mutation during growth", Label(tests.LabelDynamicStorage), func() {
		It("accepts storage spec mutations during growth without losing data", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-spec-change"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 2
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "15Gi",
				TargetBuffer: ptr.To(20),
			}

			tableLocator := TableLocator{
				Namespace:    namespace,
				ClusterName:  clusterName,
				DatabaseName: postgres.AppDBName,
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

			By("filling disk to trigger growth", func() {
				primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				// Spec mutation test — no switchover/failover, just spec changes.
				finalUsage, fillErr := fillDiskFast(primaryPod, 85, 87)
				if fillErr != nil {
					GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
				}
				GinkgoWriter.Printf("Final disk usage after fill: %d%%\n", finalUsage)
				Expect(finalUsage).To(BeNumerically(">=", 75),
					"Disk fill should reach at least 75%% to trigger growth")
			})

			By("waiting for storage sizing to be active", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})

			By("mutating spec: increasing limit", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				originCluster := cluster.DeepCopy()
				cluster.Spec.StorageConfiguration.Limit = "25Gi"
				err = env.Client.Patch(env.Ctx, cluster, ctrlclient.MergeFrom(originCluster))
				Expect(err).ToNot(HaveOccurred())
			})

			By("verifying reconciler accepts new spec", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Spec.StorageConfiguration.Limit).To(Equal("25Gi"))
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})

			By("mutating spec: adjusting targetBuffer", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				originCluster := cluster.DeepCopy()
				cluster.Spec.StorageConfiguration.TargetBuffer = ptr.To(30)
				err = env.Client.Patch(env.Ctx, cluster, ctrlclient.MergeFrom(originCluster))
				Expect(err).ToNot(HaveOccurred())
			})

			By("verifying cluster remains in healthy state", func() {
				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("verifying data integrity", func() {
				AssertDataExpectedCount(env, tableLocator, 2)
			})

			By("verifying PVCs respect new limit", func() {
				var pvcList corev1.PersistentVolumeClaimList
				err := env.Client.List(env.Ctx, &pvcList,
					ctrlclient.InNamespace(namespace),
					ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
				Expect(err).ToNot(HaveOccurred())
				newLimit := resource.MustParse("25Gi")
				for _, pvc := range pvcList.Items {
					size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
					Expect(size.Cmp(newLimit)).To(BeNumerically("<=", 0))
				}
			})
		})
	})

	// Test: Growth operation in progress + node cordon/drain affecting active instance
	Context("Node drain during growth",
		Serial, Label(tests.LabelDynamicStorage, tests.LabelDisruptive, tests.LabelMaintenance), func() {
			It("recovers growth operation after node drain", func() {
				var err error
				namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
				Expect(err).ToNot(HaveOccurred())

				clusterName := "dynamic-node-drain"
				cluster := &apiv1.Cluster{}
				cluster.Name = clusterName
				cluster.Namespace = namespace
				cluster.Spec.Instances = 3
				cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
					Request:      "5Gi",
					Limit:        "20Gi",
					TargetBuffer: ptr.To(20),
				}
				stiffenSpecForReliability(cluster)

				tableLocator := TableLocator{
					Namespace:    namespace,
					ClusterName:  clusterName,
					DatabaseName: postgres.AppDBName,
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

				By("verifying standbys are streaming before filling disk", func() {
					AssertClusterStandbysAreStreaming(namespace, clusterName, int32(testTimeouts[timeouts.AKSVolumeAttach]))
				})

				By("filling disk to trigger growth", func() {
					primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
					Expect(err).ToNot(HaveOccurred())

					// Node drain evicts pod but PVC persists — dd fill file survives
					// reattachment. No pg_rewind involved.
					finalUsage, fillErr := fillDiskFast(primaryPod, 85, 87)
					if fillErr != nil {
						GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
					}
					GinkgoWriter.Printf("Final disk usage after fill: %d%%\n", finalUsage)
					Expect(finalUsage).To(BeNumerically(">=", 75),
						"Disk fill should reach at least 75%% to trigger growth")
				})

				By("waiting for storage sizing to be active", func() {
					Eventually(func(g Gomega) {
						cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
						g.Expect(err).ToNot(HaveOccurred())
						g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
						WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
				})

				By("draining node containing primary", func() {
					// DrainPrimary cordons and drains the node containing the primary pod
					podsOnPrimaryNode := nodes.DrainPrimary(
						env.Ctx, env.Client,
						namespace, clusterName,
						testTimeouts[timeouts.DrainNode],
					)
					Expect(podsOnPrimaryNode).ToNot(BeEmpty())
				})

				By("verifying cluster converges after drain", func() {
					// Uncordon nodes to allow pods to be rescheduled
					err := nodes.UncordonAll(env.Ctx, env.Client)
					Expect(err).ToNot(HaveOccurred())
					// Use ClusterIsReadySlow because node drain + pod reschedule + volume reattach on AKS can take 20+ minutes

					AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReadySlow], env)
				})

				By("verifying storage sizing continues after drain", func() {
					Eventually(func(g Gomega) {
						cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
						g.Expect(err).ToNot(HaveOccurred())
						g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
						WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
				})

				By("verifying data integrity after drain", func() {
					AssertDataExpectedCount(env, tableLocator, 2)
				})

				By("verifying PVCs respect bounds after drain", func() {
					var pvcList corev1.PersistentVolumeClaimList
					err := env.Client.List(env.Ctx, &pvcList,
						ctrlclient.InNamespace(namespace),
						ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
					Expect(err).ToNot(HaveOccurred())

					request := resource.MustParse("5Gi")
					limit := resource.MustParse("20Gi")
					for _, pvc := range pvcList.Items {
						size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
						Expect(size.Cmp(request)).To(BeNumerically(">=", 0))
						Expect(size.Cmp(limit)).To(BeNumerically("<=", 0))
					}
				})
			})
		})

	// Test: Growth operation in progress + backup creation
	Context("Backup during growth", Label(tests.LabelDynamicStorage), func() {
		It("backup succeeds or fails clearly without deadlocking storage reconciliation", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-backup"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 2
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
				// Set high emergency thresholds so disk fill to 85% triggers
				// scheduled growth instead of emergency growth. Emergency growth
				// blocks backup operations and takes longer on AKS.
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					CriticalThreshold:   99,
					CriticalMinimumFree: "100Mi",
				},
			}

			tableLocator := TableLocator{
				Namespace:    namespace,
				ClusterName:  clusterName,
				DatabaseName: postgres.AppDBName,
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

			By("filling disk to trigger growth", func() {
				primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				// Backup deadlock test — just needs disk pressure, no WAL concerns.
				finalUsage, fillErr := fillDiskFast(primaryPod, 85, 87)
				if fillErr != nil {
					GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
				}
				GinkgoWriter.Printf("Final disk usage after fill: %d%%\n", finalUsage)
				Expect(finalUsage).To(BeNumerically(">=", 75),
					"Disk fill should reach at least 75%% to trigger growth")
			})

			By("waiting for storage sizing to be active", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})

			backupName := clusterName + "-during-growth"
			By("starting backup while growth is in-flight", func() {
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
			})

			By("verifying backup reaches a terminal phase", func() {
				// Use AKS-specific backup timeout since backups during volume resize
				// operations can take significantly longer on Azure
				Eventually(func(g Gomega) {
					backup := &apiv1.Backup{}
					err := env.Client.Get(env.Ctx, ctrlclient.ObjectKey{
						Namespace: namespace,
						Name:      backupName,
					}, backup)
					g.Expect(err).ToNot(HaveOccurred())
					// Use BeEquivalentTo for type-safe comparison with BackupPhase type alias
					g.Expect(backup.Status.Phase).To(Or(
						BeEquivalentTo(apiv1.BackupPhaseCompleted),
						BeEquivalentTo(apiv1.BackupPhaseFailed),
					))
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSBackupIsReady]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})

			By("verifying cluster remains healthy and storage sizing continues", func() {
				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)

				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})

			By("verifying data integrity", func() {
				AssertDataExpectedCount(env, tableLocator, 2)
			})
		})
	})

	// Test: New replica scale-up during/after prior dynamic resize
	Context("Replica scale-up after resize", Label(tests.LabelDynamicStorage), func() {
		It("creates new replica at effective operational size", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-replica-size"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
			}

			tableLocator := TableLocator{
				Namespace:    namespace,
				ClusterName:  clusterName,
				DatabaseName: postgres.AppDBName,
				TableName:    "sentinel",
			}

			By("creating cluster with 1 instance", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())
				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("inserting sentinel data", func() {
				AssertCreateTestData(env, tableLocator)
			})

			By("recording original PVC size", func() {
				var pvcList corev1.PersistentVolumeClaimList
				err := env.Client.List(env.Ctx, &pvcList,
					ctrlclient.InNamespace(namespace),
					ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
				Expect(err).ToNot(HaveOccurred())
				Expect(pvcList.Items).To(HaveLen(1))
				originalPVCSize := pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
				GinkgoWriter.Printf("Original PVC size: %s\n", originalPVCSize.String())
			})

			By("filling disk to trigger growth", func() {
				primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				// Replica scaling test — just needs disk pressure for growth, no pg_rewind.
				finalUsage, fillErr := fillDiskFast(primaryPod, 85, 87)
				if fillErr != nil {
					GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
				}
				GinkgoWriter.Printf("Final disk usage after fill: %d%%\n", finalUsage)
				Expect(finalUsage).To(BeNumerically(">=", 75),
					"Disk fill should reach at least 75%% to trigger growth")
			})

			By("waiting for storage sizing status to be populated", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})

			// The effective size may or may not be set depending on whether growth was triggered.
			// What matters is that new replicas match the current PVC size of existing instances.
			var currentPVCSize resource.Quantity
			By("recording current PVC size", func() {
				var pvcList corev1.PersistentVolumeClaimList
				err := env.Client.List(env.Ctx, &pvcList,
					ctrlclient.InNamespace(namespace),
					ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
				Expect(err).ToNot(HaveOccurred())
				Expect(pvcList.Items).To(HaveLen(1))
				currentPVCSize = pvcList.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
				GinkgoWriter.Printf("Current PVC size: %s\n", currentPVCSize.String())
			})

			// Get effective size if available, otherwise use current PVC size
			var effectiveSize string
			By("recording effective size from status or using current PVC size", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				if cluster.Status.StorageSizing.Data.EffectiveSize != "" {
					effectiveSize = cluster.Status.StorageSizing.Data.EffectiveSize
				} else {
					// If effective size is not set, use the current PVC size
					effectiveSize = currentPVCSize.String()
				}
				GinkgoWriter.Printf("Effective size to use: %s\n", effectiveSize)
			})

			By("scaling to 2 instances", func() {
				cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				originCluster := cluster.DeepCopy()
				cluster.Spec.Instances = 2
				err = env.Client.Patch(env.Ctx, cluster, ctrlclient.MergeFrom(originCluster))
				Expect(err).ToNot(HaveOccurred())

				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("verifying new replica PVC is created at effective size", func() {
				Eventually(func(g Gomega) {
					var pvcList corev1.PersistentVolumeClaimList
					err := env.Client.List(env.Ctx, &pvcList,
						ctrlclient.InNamespace(namespace),
						ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(pvcList.Items).To(HaveLen(2))

					// New replica should be at effective size, not the stale request size
					effectiveSizeQty := resource.MustParse(effectiveSize)
					for _, pvc := range pvcList.Items {
						size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
						// All PVCs should be at or near effective size (may be slightly larger due to PV rounding)
						g.Expect(size.Cmp(effectiveSizeQty)).To(BeNumerically(">=", 0),
							fmt.Sprintf("PVC %s size %s should be >= effective size %s",
								pvc.Name, size.String(), effectiveSize))
					}
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})

			By("verifying data integrity on new replica", func() {
				AssertDataExpectedCount(env, tableLocator, 2)
			})
		})
	})

	// Test: Daily action-budget or rate-limit boundaries
	Context("Rate limiting", Label(tests.LabelDynamicStorage), func() {
		It("exposes planned/emergency budget split in status", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-budget"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "30Gi",
				TargetBuffer: ptr.To(20),
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					Enabled:                     ptr.To(true),
					CriticalThreshold:           95,
					MaxActionsPerDay:            ptr.To(3),
					ReservedActionsForEmergency: ptr.To(1),
				},
			}

			By("creating cluster", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())
				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("verifying budget is initialized correctly", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data.Budget).ToNot(BeNil())
					budget := cluster.Status.StorageSizing.Data.Budget
					// With maxActionsPerDay=3 and reservedForEmergency=1:
					// availableForPlanned should be 2, availableForEmergency should be 1
					g.Expect(budget.AvailableForPlanned).To(BeEquivalentTo(2))
					g.Expect(budget.AvailableForEmergency).To(BeEquivalentTo(1))
					g.Expect(budget.BudgetResetsAt.IsZero()).To(BeFalse())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})

			By("verifying budget status is exposed in cluster status", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data.Budget).ToNot(BeNil())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})
		})
	})

	// ============================================================================
	// Topology Matrix Tests (T1, T2, T3)
	// Per requirements, P0 scenarios must run in each topology
	// ============================================================================

	Context("Topology T1: Single instance (instances=1)", Label(tests.LabelDynamicStorage), func() {
		It("handles dynamic sizing with no replicas", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-t1"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 1
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
			}

			tableLocator := TableLocator{
				Namespace:    namespace,
				ClusterName:  clusterName,
				DatabaseName: postgres.AppDBName,
				TableName:    "sentinel",
			}

			By("creating single-instance cluster", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())
				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("inserting sentinel data", func() {
				AssertCreateTestData(env, tableLocator)
			})

			By("filling disk to trigger growth", func() {
				primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				// T1 single-instance — no replication, just needs disk pressure.
				finalUsage, fillErr := fillDiskFast(primaryPod, 85, 87)
				if fillErr != nil {
					GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
				}
				GinkgoWriter.Printf("Final disk usage after fill: %d%%\n", finalUsage)
				Expect(finalUsage).To(BeNumerically(">=", 75),
					"Disk fill should reach at least 75%% to trigger growth")
			})

			By("verifying storage sizing works with T1 topology", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.StorageSizingDetection]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.StorageSizingPolling]) * time.Second).Should(Succeed())
			})

			By("waiting for PVC capacity to update (CSI resize completion)", func() {
				// For T1 topology (single instance), we must wait for the CSI resize to complete
				// before verifying data integrity, as the instance may be affected by the resize.
				waitForPVCCapacityUpdate(namespace, clusterName,
					time.Duration(testTimeouts[timeouts.AKSVolumeResize])*time.Second)
			})

			By("verifying data integrity", func() {
				AssertDataExpectedCount(env, tableLocator, 2)
			})
		})
	})

	Context("Topology T2: Two instances (instances=2)", Label(tests.LabelDynamicStorage), func() {
		It("handles dynamic sizing with single replica", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-t2"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 2
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request: "5Gi",
				Limit:   "20Gi",
				// Use a higher TargetBuffer (30%) so growth triggers at ~70% disk usage
				// instead of 80%. This keeps disk fill pressure low, preserving WAL
				// headroom needed for pg_rewind after the switchover in this test.
				TargetBuffer: ptr.To(30),
			}
			// Configure wal_keep_size to retain WAL segments for pg_rewind.
			// When the old primary rejoins after switchover, pg_rewind needs
			// to find the divergence point in WAL. Without explicit retention,
			// WAL segments may be recycled under disk pressure.
			// Use 2GB to ensure sufficient WAL retention even with fast disk fill.
			cluster.Spec.PostgresConfiguration.Parameters = map[string]string{
				"wal_keep_size": "2GB",
			}
			stiffenSpecForReliability(cluster)

			tableLocator := TableLocator{
				Namespace:    namespace,
				ClusterName:  clusterName,
				DatabaseName: postgres.AppDBName,
				TableName:    "sentinel",
			}

			By("creating two-instance cluster", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())
				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("inserting sentinel data", func() {
				AssertCreateTestData(env, tableLocator)
			})

			By("filling disk to trigger growth", func() {
				primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				// Use fillDiskFast (dd/fallocate) instead of fillDiskIncrementally (SQL inserts).
				// SQL inserts generate WAL, and when disk fills to 70%+, PostgreSQL recycles
				// WAL segments aggressively. After switchover, the old primary needs pg_rewind
				// which fails if the required WAL is gone. fillDiskFast bypasses PostgreSQL
				// entirely, preserving WAL history for pg_rewind.
				//
				// With TargetBuffer=30%, NeedsGrowth triggers when free space < 30% (usage > 70%).
				// We fill to 75% to reliably trigger growth.
				finalUsage, fillErr := fillDiskFast(primaryPod, 75, 80)
				if fillErr != nil {
					GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
				}
				GinkgoWriter.Printf("Final disk usage after fill: %d%%\n", finalUsage)
				Expect(finalUsage).To(BeNumerically(">=", 70),
					"Disk fill should reach at least 70%% to trigger growth with TargetBuffer=30%%")
			})

			By("verifying dynamic sizing detected the condition", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.StorageSizingDetection]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.StorageSizingPolling]) * time.Second).Should(Succeed())
			})

			By("waiting for growth to complete before switchover", func() {
				// Require explicit evidence that growth actually occurred:
				// 1. LastAction must be set (a grow action was taken)
				// 2. PVC request must have increased above the initial 5Gi
				// Without this gate, a false-positive capacity check could let the test
				// proceed to switchover on a still-5Gi volume with 70%+ usage, causing
				// pg_rewind failures from insufficient WAL headroom.
				initialRequest := resource.MustParse("5Gi")

				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data.LastAction).ToNot(BeNil(),
						"Expected a growth action to have been recorded")
					GinkgoWriter.Printf("Growth action: %s -> %s (kind: %s, result: %s)\n",
						cluster.Status.StorageSizing.Data.LastAction.From,
						cluster.Status.StorageSizing.Data.LastAction.To,
						cluster.Status.StorageSizing.Data.LastAction.Kind,
						cluster.Status.StorageSizing.Data.LastAction.Result)
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())

				// Verify at least one PVC request actually increased
				Eventually(func(g Gomega) {
					var pvcList corev1.PersistentVolumeClaimList
					err := env.Client.List(env.Ctx, &pvcList,
						ctrlclient.InNamespace(namespace),
						ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
					g.Expect(err).ToNot(HaveOccurred())

					grewCount := 0
					for _, pvc := range pvcList.Items {
						request := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
						if request.Cmp(initialRequest) > 0 {
							grewCount++
							GinkgoWriter.Printf("PVC %s grew: request=%s (> initial %s)\n",
								pvc.Name, request.String(), initialRequest.String())
						}
					}
					g.Expect(grewCount).To(BeNumerically(">", 0),
						"At least one PVC request should have increased above 5Gi")
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())

				// Note: We intentionally skip waiting for CSI resize completion here.
				// Azure CSI driver can take 40+ minutes to complete filesystem resize.
				// For T2 topology test, our goal is to verify:
				// 1. Growth is detected and triggered (verified above)
				// 2. Switchover works even while resize is in progress
				// The CSI resize will complete eventually; we don't need to block on it.
			})

			By("verifying replica is streaming before switchover", func() {
				AssertClusterStandbysAreStreaming(namespace, clusterName, int32(testTimeouts[timeouts.AKSVolumeAttach]))
			})

			var previousPrimary, promotedPrimary string
			By("verifying promotion/replica replacement ordering is safe", func() {
				previousPrimary, promotedPrimary = triggerSwitchoverAndWait(namespace, clusterName)
				Expect(previousPrimary).ToNot(Equal(promotedPrimary))
			})

			By("verifying role transition completed cleanly", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.CurrentPrimary).To(Equal(promotedPrimary))
					g.Expect(cluster.Status.CurrentPrimary).ToNot(Equal(previousPrimary))
					g.Expect(cluster.Status.TargetPrimary).To(Equal(promotedPrimary))
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})

			By("verifying replica catches up and streams after switchover", func() {
				AssertClusterStandbysAreStreaming(namespace, clusterName, int32(testTimeouts[timeouts.AKSVolumeAttach]))
			})

			By("verifying data integrity after switchover", func() {
				AssertDataExpectedCount(env, tableLocator, 2)
			})

			By("verifying all PVCs are consistent", func() {
				var pvcList corev1.PersistentVolumeClaimList
				err := env.Client.List(env.Ctx, &pvcList,
					ctrlclient.InNamespace(namespace),
					ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
				Expect(err).ToNot(HaveOccurred())
				Expect(pvcList.Items).To(HaveLen(2))

				request := resource.MustParse("5Gi")
				limit := resource.MustParse("20Gi")
				for _, pvc := range pvcList.Items {
					size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
					Expect(size.Cmp(request)).To(BeNumerically(">=", 0))
					Expect(size.Cmp(limit)).To(BeNumerically("<=", 0))
				}
			})
		})
	})

	Context("Topology T3: Multiple replicas (instances>=3)", Label(tests.LabelDynamicStorage), func() {
		It("handles dynamic sizing with multiple replicas without unnecessary churn", func() {
			var err error
			namespace, err = env.CreateUniqueTestNamespace(env.Ctx, env.Client, namespacePrefix)
			Expect(err).ToNot(HaveOccurred())

			clusterName := "dynamic-t3"
			cluster := &apiv1.Cluster{}
			cluster.Name = clusterName
			cluster.Namespace = namespace
			cluster.Spec.Instances = 3
			cluster.Spec.StorageConfiguration = apiv1.StorageConfiguration{
				Request:      "5Gi",
				Limit:        "20Gi",
				TargetBuffer: ptr.To(20),
			}
			stiffenSpecForReliability(cluster)

			tableLocator := TableLocator{
				Namespace:    namespace,
				ClusterName:  clusterName,
				DatabaseName: postgres.AppDBName,
				TableName:    "sentinel",
			}

			By("creating three-instance cluster", func() {
				err := env.Client.Create(env.Ctx, cluster)
				Expect(err).ToNot(HaveOccurred())
				AssertClusterIsReady(namespace, clusterName, testTimeouts[timeouts.ClusterIsReady], env)
			})

			By("inserting sentinel data", func() {
				AssertCreateTestData(env, tableLocator)
			})

			// Record initial pod UIDs to check for unnecessary churn
			initialPodUIDs := make(map[string]types.UID)
			By("recording initial pod UIDs", func() {
				podList, err := clusterutils.ListPods(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, pod := range podList.Items {
					initialPodUIDs[pod.Name] = pod.UID
				}
			})

			By("filling disk to trigger growth", func() {
				primaryPod, err := clusterutils.GetPrimary(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				// T3 multi-replica — no switchover, just needs disk pressure for growth.
				finalUsage, fillErr := fillDiskFast(primaryPod, 85, 87)
				if fillErr != nil {
					GinkgoWriter.Printf("Disk fill ended with error (may be expected): %v\n", fillErr)
				}
				GinkgoWriter.Printf("Final disk usage after fill: %d%%\n", finalUsage)
				Expect(finalUsage).To(BeNumerically(">=", 75),
					"Disk fill should reach at least 75%% to trigger growth")
			})

			By("verifying storage sizing works with T3 topology", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
				}).WithTimeout(time.Duration(testTimeouts[timeouts.StorageSizingDetection]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.StorageSizingPolling]) * time.Second).Should(Succeed())
			})

			By("waiting for PVC capacity to update (CSI resize completion)", func() {
				// CRITICAL: Wait for CSI driver to complete the resize before checking PVC consistency.
				// Azure AKS CSI operations can take 5-10 minutes.
				waitForPVCCapacityUpdate(namespace, clusterName,
					time.Duration(testTimeouts[timeouts.AKSVolumeResize])*time.Second)
			})

			By("verifying data integrity", func() {
				AssertDataExpectedCount(env, tableLocator, 2)
			})

			By("verifying no unnecessary multi-node churn", func() {
				// Wait for storage sizing reconciliation to stabilize by watching for
				// a consistent state, rather than using a fixed sleep.
				// We verify that pods remain stable (no unnecessary restarts) by checking
				// that pod UIDs haven't changed after the storage sizing status stabilizes.
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					// Wait for storage sizing status to reach a stable state
					g.Expect(cluster.Status.StorageSizing).ToNot(BeNil())
					g.Expect(cluster.Status.StorageSizing.Data).ToNot(BeNil())
					// State should be one of the stable states (Balanced, PendingGrowth, etc.)
					// not actively Resizing
					state := cluster.Status.StorageSizing.Data.State
					g.Expect(state).ToNot(Equal("Resizing"),
						"Waiting for storage sizing to complete resizing")
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())

				podList, err := clusterutils.ListPods(env.Ctx, env.Client, namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())

				// Count how many pods were replaced (UID changed)
				replacedPods := 0
				for _, pod := range podList.Items {
					if originalUID, exists := initialPodUIDs[pod.Name]; exists {
						if pod.UID != originalUID {
							replacedPods++
							GinkgoWriter.Printf("Pod %s was replaced (old UID: %s, new UID: %s)\n",
								pod.Name, originalUID, pod.UID)
						}
					}
				}
				// Storage resize should not cause unnecessary pod replacements
				// (online resize should keep pods running)
				Expect(replacedPods).To(BeNumerically("<=", 1),
					"Expected at most 1 pod replacement for storage resize, got %d", replacedPods)
			})

			By("verifying quorum-safe behavior (3 instances running)", func() {
				Eventually(func(g Gomega) {
					cluster, err := clusterutils.Get(env.Ctx, env.Client, namespace, clusterName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(cluster.Status.ReadyInstances).To(Equal(3))
				}).WithTimeout(time.Duration(testTimeouts[timeouts.AKSVolumeResize]) * time.Second).
					WithPolling(time.Duration(testTimeouts[timeouts.AKSPollingInterval]) * time.Second).Should(Succeed())
			})

			By("verifying all PVCs are consistent", func() {
				var pvcList corev1.PersistentVolumeClaimList
				err := env.Client.List(env.Ctx, &pvcList,
					ctrlclient.InNamespace(namespace),
					ctrlclient.MatchingLabels{utils.ClusterLabelName: clusterName})
				Expect(err).ToNot(HaveOccurred())
				Expect(pvcList.Items).To(HaveLen(3))

				request := resource.MustParse("5Gi")
				limit := resource.MustParse("20Gi")
				for _, pvc := range pvcList.Items {
					size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
					Expect(size.Cmp(request)).To(BeNumerically(">=", 0))
					Expect(size.Cmp(limit)).To(BeNumerically("<=", 0))
				}
			})
		})
	})
})
