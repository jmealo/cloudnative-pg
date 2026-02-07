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

package autoresize

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudnative-pg/machinery/pkg/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"
	pvcresources "github.com/cloudnative-pg/cloudnative-pg/pkg/resources"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
)

const (
	// RequeueDelay is the delay after a resize operation before the next check.
	RequeueDelay = 30 * time.Second

	// MonitoringInterval is the interval for routine disk usage monitoring
	// when auto-resize is enabled but no action was taken. This ensures we
	// detect disk fills that happen asynchronously.
	MonitoringInterval = 30 * time.Second

	// DefaultMaxActionsPerDay is the default rate limit for resize operations.
	DefaultMaxActionsPerDay = 3
)

// GlobalBudgetTracker is the shared budget tracker across reconciliation cycles.
var GlobalBudgetTracker = NewBudgetTracker()

// InstanceDiskInfo holds the disk status info extracted from an instance's PostgresqlStatus.
type InstanceDiskInfo struct {
	DiskStatus      *postgres.DiskStatus
	WALHealthStatus *postgres.WALHealthStatus
}

// Reconcile evaluates all PVCs in the cluster for auto-resize eligibility
// and performs PVC patching when conditions are met.
// Decision flow: trigger check -> budget check -> limit check -> WAL safety -> clamp -> patch PVC -> event.
func Reconcile(
	ctx context.Context,
	c client.Client,
	cluster *apiv1.Cluster,
	diskInfoByPod map[string]*InstanceDiskInfo,
	pvcs []corev1.PersistentVolumeClaim,
) (ctrl.Result, error) {
	contextLogger := log.FromContext(ctx).WithName("autoresize")

	// Check if any storage has resize enabled
	if !IsAutoResizeEnabled(cluster) {
		return ctrl.Result{}, nil
	}

	contextLogger.Debug("auto-resize reconciler running",
		"pvcCount", len(pvcs),
		"diskInfoPodCount", len(diskInfoByPod))

	// Log disk info for debugging
	for podName, info := range diskInfoByPod {
		if info != nil && info.DiskStatus != nil && info.DiskStatus.DataVolume != nil {
			contextLogger.Debug("pod disk status",
				"pod", podName,
				"percentUsed", info.DiskStatus.DataVolume.PercentUsed,
				"availableBytes", info.DiskStatus.DataVolume.AvailableBytes)
		} else {
			contextLogger.Debug("pod disk status missing",
				"pod", podName,
				"hasInfo", info != nil,
				"hasDiskStatus", info != nil && info.DiskStatus != nil)
		}
	}

	var resizedAny bool

	for idx := range pvcs {
		pvc := &pvcs[idx]
		resized, err := reconcilePVC(ctx, c, cluster, diskInfoByPod, pvc)
		if err != nil {
			contextLogger.Error(err, "failed to auto-resize PVC",
				"pvcName", pvc.Name)
			continue
		}
		if resized {
			resizedAny = true
		}
	}

	// When a resize was performed, request a requeue after a short delay to
	// verify the resize completed successfully.
	//
	// IMPORTANT: We do NOT return MonitoringInterval here, even when diskInfoByPod
	// has entries. Returning non-zero at this point in the cluster_controller.go
	// reconciliation (line 815) would cause an early return BEFORE reconcilePods
	// and RegisterPhase, preventing the cluster from ever reaching healthy status.
	//
	// Instead, we rely on the cluster controller to manage the monitoring requeue
	// at the end of its reconciliation loop, after the cluster is healthy.
	if resizedAny {
		return ctrl.Result{RequeueAfter: RequeueDelay}, nil
	}

	return ctrl.Result{}, nil
}

// IsAutoResizeEnabled checks if auto-resize is enabled for any storage in the cluster.
// This is exported so the cluster controller can use it to determine if monitoring
// reconciliation is needed.
func IsAutoResizeEnabled(cluster *apiv1.Cluster) bool {
	if cluster.Spec.StorageConfiguration.Resize != nil &&
		cluster.Spec.StorageConfiguration.Resize.Enabled {
		return true
	}

	if cluster.Spec.WalStorage != nil &&
		cluster.Spec.WalStorage.Resize != nil &&
		cluster.Spec.WalStorage.Resize.Enabled {
		return true
	}

	for _, tbs := range cluster.Spec.Tablespaces {
		if tbs.Storage.Resize != nil && tbs.Storage.Resize.Enabled {
			return true
		}
	}

	return false
}

// reconcilePVC evaluates a single PVC for auto-resize.
func reconcilePVC(
	ctx context.Context,
	c client.Client,
	cluster *apiv1.Cluster,
	diskInfoByPod map[string]*InstanceDiskInfo,
	pvc *corev1.PersistentVolumeClaim,
) (bool, error) {
	contextLogger := log.FromContext(ctx)

	// Determine the PVC role and get the corresponding resize configuration
	pvcRole := pvc.Labels[utils.PvcRoleLabelName]
	resizeConfig := getResizeConfigForPVC(cluster, pvc)
	if resizeConfig == nil || !resizeConfig.Enabled {
		return false, nil
	}

	// Get the pod name for this PVC
	podName := pvc.Labels[utils.InstanceNameLabelName]
	if podName == "" {
		return false, nil
	}

	// Get disk status for this instance
	diskInfo, ok := diskInfoByPod[podName]
	if !ok || diskInfo == nil || diskInfo.DiskStatus == nil {
		return false, nil
	}

	// Get volume stats for this PVC
	volumeStats := getVolumeStatsForPVC(diskInfo.DiskStatus, pvcRole, pvc)
	if volumeStats == nil {
		return false, nil
	}

	// 1. Trigger check
	triggers := resizeConfig.Triggers
	if triggers == nil {
		triggers = &apiv1.ResizeTriggers{UsageThreshold: 80}
	}

	// nolint:gosec // G115: disk sizes won't exceed int64 limits (9.2 EB)
	if !ShouldResize(volumeStats.PercentUsed, int64(volumeStats.AvailableBytes), triggers) {
		return false, nil
	}

	contextLogger.Info("auto-resize trigger fired",
		"pvc", pvc.Name,
		"percentUsed", volumeStats.PercentUsed,
		"availableBytes", volumeStats.AvailableBytes,
	)

	// 2. Budget check
	maxActionsPerDay := DefaultMaxActionsPerDay
	if resizeConfig.Strategy != nil && resizeConfig.Strategy.MaxActionsPerDay > 0 {
		maxActionsPerDay = resizeConfig.Strategy.MaxActionsPerDay
	}

	volumeKey := fmt.Sprintf("%s/%s", cluster.Namespace, pvc.Name)
	if !GlobalBudgetTracker.HasBudget(volumeKey, maxActionsPerDay) {
		contextLogger.Info("auto-resize blocked: rate limit exceeded",
			"pvc", pvc.Name, "maxActionsPerDay", maxActionsPerDay)
		return false, nil
	}

	// 3. Limit check
	expansion := resizeConfig.Expansion
	if expansion == nil {
		expansion = &apiv1.ExpansionPolicy{}
	}

	currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if expansion.Limit != "" {
		limit, err := resource.ParseQuantity(expansion.Limit)
		if err == nil && currentSize.Cmp(limit) >= 0 {
			contextLogger.Info("auto-resize blocked: at expansion limit",
				"pvc", pvc.Name, "currentSize", currentSize.String(), "limit", limit.String())
			return false, nil
		}
	}

	// 4. WAL safety check
	isSingleVolume := !cluster.ShouldCreateWalArchiveVolume()
	var walSafety *apiv1.WALSafetyPolicy
	if resizeConfig.Strategy != nil {
		walSafety = resizeConfig.Strategy.WALSafetyPolicy
	}

	walSafetyResult := EvaluateWALSafety(pvcRole, isSingleVolume, walSafety, diskInfo.WALHealthStatus)
	if !walSafetyResult.Allowed {
		contextLogger.Info(walSafetyResult.BlockMessage,
			"pvc", pvc.Name,
			"reason", string(walSafetyResult.BlockReason),
		)
		return false, nil
	}

	// 5. Calculate new size (clamp)
	newSize, err := CalculateNewSize(currentSize, expansion)
	if err != nil {
		return false, fmt.Errorf("failed to calculate new size: %w", err)
	}

	if newSize.Cmp(currentSize) <= 0 {
		contextLogger.Debug("auto-resize: no size change needed",
			"pvc", pvc.Name, "currentSize", currentSize.String(), "newSize", newSize.String())
		return false, nil
	}

	// 6. Patch PVC
	contextLogger.Info("auto-resizing PVC",
		"pvc", pvc.Name,
		"from", currentSize.String(),
		"to", newSize.String(),
	)

	oldPVC := pvc.DeepCopy()
	pvc = pvcresources.NewPersistentVolumeClaimBuilderFromPVC(pvc).
		WithRequests(corev1.ResourceList{corev1.ResourceStorage: newSize}).
		Build()

	if err := c.Patch(ctx, pvc, client.MergeFrom(oldPVC)); err != nil {
		return false, fmt.Errorf("failed to patch PVC %s: %w", pvc.Name, err)
	}

	// 7. Record the resize
	GlobalBudgetTracker.RecordResize(volumeKey)

	// Record event in cluster status
	event := apiv1.AutoResizeEvent{
		Timestamp:    metav1.Now(),
		InstanceName: podName,
		VolumeType:   pvcRole,
		PreviousSize: currentSize.String(),
		NewSize:      newSize.String(),
		Result:       "success",
	}
	appendResizeEvent(cluster, event)

	return true, nil
}

// getResizeConfigForPVC returns the resize configuration for the given PVC.
func getResizeConfigForPVC(cluster *apiv1.Cluster, pvc *corev1.PersistentVolumeClaim) *apiv1.ResizeConfiguration {
	pvcRole := pvc.Labels[utils.PvcRoleLabelName]

	switch pvcRole {
	case string(utils.PVCRolePgData):
		return cluster.Spec.StorageConfiguration.Resize
	case string(utils.PVCRolePgWal):
		if cluster.Spec.WalStorage != nil {
			return cluster.Spec.WalStorage.Resize
		}
	case string(utils.PVCRolePgTablespace):
		tbsName := pvc.Labels[utils.TablespaceNameLabelName]
		for _, tbs := range cluster.Spec.Tablespaces {
			if tbs.Name == tbsName {
				return tbs.Storage.Resize
			}
		}
	}

	return nil
}

// getVolumeStatsForPVC returns the volume stats for the given PVC.
func getVolumeStatsForPVC(
	diskStatus *postgres.DiskStatus,
	pvcRole string,
	pvc *corev1.PersistentVolumeClaim,
) *postgres.VolumeStatus {
	switch pvcRole {
	case string(utils.PVCRolePgData):
		return diskStatus.DataVolume
	case string(utils.PVCRolePgWal):
		return diskStatus.WALVolume
	case string(utils.PVCRolePgTablespace):
		tbsName := pvc.Labels[utils.TablespaceNameLabelName]
		if diskStatus.Tablespaces != nil {
			return diskStatus.Tablespaces[tbsName]
		}
	}

	return nil
}

// appendResizeEvent appends a resize event to the cluster status.
// It keeps at most 50 events in the history.
func appendResizeEvent(cluster *apiv1.Cluster, event apiv1.AutoResizeEvent) {
	cluster.Status.AutoResizeEvents = append(cluster.Status.AutoResizeEvents, event)
	if len(cluster.Status.AutoResizeEvents) > 50 {
		cluster.Status.AutoResizeEvents = cluster.Status.AutoResizeEvents[len(cluster.Status.AutoResizeEvents)-50:]
	}
}
