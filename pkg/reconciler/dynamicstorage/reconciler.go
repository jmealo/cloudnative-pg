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
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
)

// ActionType represents the type of sizing action to take.
type ActionType string

const (
	// ActionNoOp indicates no action is needed.
	ActionNoOp ActionType = "NoOp"

	// ActionEmergencyGrow indicates an emergency growth is needed.
	ActionEmergencyGrow ActionType = "EmergencyGrow"

	// ActionScheduledGrow indicates a scheduled growth during maintenance window.
	ActionScheduledGrow ActionType = "ScheduledGrow"

	// ActionPendingGrowth indicates growth is needed but waiting for maintenance window.
	ActionPendingGrowth ActionType = "PendingGrowth"
)

// VolumeType represents the type of volume being managed.
type VolumeType string

const (
	// VolumeTypeData represents the main data volume.
	VolumeTypeData VolumeType = "data"

	// VolumeTypeWAL represents the WAL volume.
	VolumeTypeWAL VolumeType = "wal"

	// VolumeTypeTablespace represents a tablespace volume.
	VolumeTypeTablespace VolumeType = "tablespace"
)

// ReconcileResult contains the result of a sizing evaluation.
type ReconcileResult struct {
	Action       ActionType
	TargetSize   resource.Quantity
	CurrentSize  resource.Quantity
	Reason       string
	VolumeType   VolumeType
	InstanceName string
}

// DiskInfo contains disk usage information for an instance.
type DiskInfo struct {
	TotalBytes     uint64
	UsedBytes      uint64
	AvailableBytes uint64
	PercentUsed    float64
}

// Reconcile performs dynamic storage reconciliation for a cluster.
// It evaluates disk usage across all instances and triggers growth when needed.
func Reconcile(
	ctx context.Context,
	c client.Client,
	cluster *apiv1.Cluster,
	_ []corev1.Pod,
	instanceStatuses *postgres.PostgresqlStatusList,
	pvcs []corev1.PersistentVolumeClaim,
) (ctrl.Result, error) {
	contextLogger := log.FromContext(ctx)

	// Check if dynamic sizing is enabled for any volume
	if !IsDynamicSizingEnabled(&cluster.Spec.StorageConfiguration) {
		return ctrl.Result{}, nil
	}

	// Collect disk status from instance statuses
	diskStatusMap := collectDiskStatus(instanceStatuses)
	if len(diskStatusMap) == 0 {
		// If we have instance statuses but no disk status yet, they might still be initializing.
		// Requeue after a short delay to retry collecting disk status.
		if instanceStatuses != nil && len(instanceStatuses.Items) > 0 {
			contextLogger.Info("No disk status available yet, will retry",
				"instanceCount", len(instanceStatuses.Items),
				"requeueAfter", "30s")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		// No instances at all, nothing to do
		contextLogger.Debug("No instances available for disk status collection")
		return ctrl.Result{}, nil
	}

	// Evaluate sizing for data volume
	result := evaluateSizing(cluster, VolumeTypeData, diskStatusMap)

	// Initialize storage sizing status if needed
	if cluster.Status.StorageSizing == nil {
		cluster.Status.StorageSizing = &apiv1.StorageSizingStatus{}
	}
	if cluster.Status.StorageSizing.Data == nil {
		cluster.Status.StorageSizing.Data = &apiv1.VolumeSizingStatus{}
	}

	// Update status based on result
	updateVolumeStatus(cluster.Status.StorageSizing.Data, &cluster.Spec.StorageConfiguration, result)

	// Execute action if needed
	if result.Action != ActionNoOp && result.Action != ActionPendingGrowth {
		if err := executeAction(ctx, c, cluster, pvcs, result); err != nil {
			return ctrl.Result{}, fmt.Errorf("while executing dynamic storage action: %w", err)
		}

		contextLogger.Info("Dynamic storage action executed",
			"action", result.Action,
			"volumeType", result.VolumeType,
			"currentSize", result.CurrentSize.String(),
			"targetSize", result.TargetSize.String(),
			"reason", result.Reason)
	}

	return ctrl.Result{}, nil
}

// collectDiskStatus collects disk status from all instance statuses.
func collectDiskStatus(statuses *postgres.PostgresqlStatusList) map[string]*DiskInfo {
	if statuses == nil {
		return nil
	}

	result := make(map[string]*DiskInfo)
	for _, status := range statuses.Items {
		if status.DiskStatus == nil || status.Pod == nil {
			continue
		}
		result[status.Pod.Name] = &DiskInfo{
			TotalBytes:     status.DiskStatus.TotalBytes,
			UsedBytes:      status.DiskStatus.UsedBytes,
			AvailableBytes: status.DiskStatus.AvailableBytes,
			PercentUsed:    status.DiskStatus.PercentUsed,
		}
	}
	return result
}

// evaluateSizing evaluates the sizing needs for a volume type.
func evaluateSizing(
	cluster *apiv1.Cluster,
	volumeType VolumeType,
	diskStatusMap map[string]*DiskInfo,
) *ReconcileResult {
	cfg := &cluster.Spec.StorageConfiguration

	// Get the maximum usage across all instances
	var maxUsed, maxTotal, minAvailable uint64
	var highestUsageInstance string
	for instanceName, info := range diskStatusMap {
		if info.UsedBytes > maxUsed {
			maxUsed = info.UsedBytes
			maxTotal = info.TotalBytes
			minAvailable = info.AvailableBytes
			highestUsageInstance = instanceName
		}
	}

	if maxTotal == 0 {
		return &ReconcileResult{Action: ActionNoOp, VolumeType: volumeType}
	}

	request, err := resource.ParseQuantity(cfg.Request)
	if err != nil {
		return &ReconcileResult{
			Action: ActionNoOp, VolumeType: volumeType, Reason: fmt.Errorf("parsing request: %w", err).Error(),
		}
	}
	limit, err := resource.ParseQuantity(cfg.Limit)
	if err != nil {
		return &ReconcileResult{
			Action: ActionNoOp, VolumeType: volumeType, Reason: fmt.Errorf("parsing limit: %w", err).Error(),
		}
	}

	currentSize := *resource.NewQuantity(int64(maxTotal), resource.BinarySI) //nolint:gosec

	// Check if at limit
	if currentSize.Cmp(limit) >= 0 {
		return &ReconcileResult{
			Action:      ActionNoOp,
			VolumeType:  volumeType,
			CurrentSize: currentSize,
			TargetSize:  limit,
			Reason:      "at limit",
		}
	}

	// Check for emergency condition
	if IsEmergencyCondition(cfg, maxTotal, maxUsed, minAvailable) {
		// Get volume sizing status for budget check
		var volumeStatus *apiv1.VolumeSizingStatus
		if cluster.Status.StorageSizing != nil {
			volumeStatus = cluster.Status.StorageSizing.Data
		}

		if HasBudgetForEmergency(cfg, volumeStatus) {
			targetSize := CalculateEmergencyGrowthSize(currentSize, limit)
			return &ReconcileResult{
				Action:       ActionEmergencyGrow,
				VolumeType:   volumeType,
				CurrentSize:  currentSize,
				TargetSize:   targetSize,
				Reason:       "critical disk usage",
				InstanceName: highestUsageInstance,
			}
		}
		return &ReconcileResult{
			Action:      ActionNoOp,
			VolumeType:  volumeType,
			CurrentSize: currentSize,
			Reason:      "emergency budget exhausted",
		}
	}

	// Check if growth is needed
	if NeedsGrowth(cfg, maxTotal, maxUsed) {
		targetSize := CalculateTargetSize(maxUsed, GetTargetBuffer(cfg))
		targetSize = ClampSize(targetSize, request, limit)

		// Only grow if target is larger than current
		if targetSize.Cmp(currentSize) <= 0 {
			return &ReconcileResult{
				Action:      ActionNoOp,
				VolumeType:  volumeType,
				CurrentSize: currentSize,
				TargetSize:  targetSize,
				Reason:      "target not larger than current",
			}
		}

		var volumeStatus *apiv1.VolumeSizingStatus
		if cluster.Status.StorageSizing != nil {
			volumeStatus = cluster.Status.StorageSizing.Data
		}

		// Check maintenance window and budget
		if IsMaintenanceWindowOpen(cfg) && HasBudgetForScheduled(cfg, volumeStatus) {
			return &ReconcileResult{
				Action:       ActionScheduledGrow,
				VolumeType:   volumeType,
				CurrentSize:  currentSize,
				TargetSize:   targetSize,
				Reason:       "free space below target buffer",
				InstanceName: highestUsageInstance,
			}
		}

		return &ReconcileResult{
			Action:       ActionPendingGrowth,
			VolumeType:   volumeType,
			CurrentSize:  currentSize,
			TargetSize:   targetSize,
			Reason:       "waiting for maintenance window",
			InstanceName: highestUsageInstance,
		}
	}

	return &ReconcileResult{
		Action:      ActionNoOp,
		VolumeType:  volumeType,
		CurrentSize: currentSize,
		Reason:      "balanced",
	}
}

// updateVolumeStatus updates the volume sizing status based on the result.
func updateVolumeStatus(
	status *apiv1.VolumeSizingStatus,
	cfg *apiv1.StorageConfiguration,
	result *ReconcileResult,
) {
	// Update state
	switch result.Action {
	case ActionNoOp:
		if result.Reason == "at limit" {
			status.State = "AtLimit"
		} else {
			status.State = "Balanced"
		}
	case ActionEmergencyGrow:
		status.State = "Emergency"
	case ActionScheduledGrow:
		status.State = "Resizing"
	case ActionPendingGrowth:
		status.State = "PendingGrowth"
	}

	// Update target size
	if !result.TargetSize.IsZero() {
		status.TargetSize = result.TargetSize.String()
	}

	// Update next maintenance window
	if result.Action == ActionPendingGrowth {
		next := NextMaintenanceWindow(cfg)
		if next != nil {
			status.NextMaintenanceWindow = &metav1.Time{Time: *next}
		}
	}

	// Update budget
	status.Budget = CalculateBudget(cfg, status)
}

// executeAction executes a sizing action by patching PVCs.
func executeAction(
	ctx context.Context,
	c client.Client,
	cluster *apiv1.Cluster,
	pvcs []corev1.PersistentVolumeClaim,
	result *ReconcileResult,
) error {
	contextLogger := log.FromContext(ctx)

	// Find data PVCs and patch them
	for i := range pvcs {
		pvc := &pvcs[i]

		// Check if this is a data PVC
		role := pvc.Labels[utils.PvcRoleLabelName]
		if role != string(utils.PVCRolePgData) {
			continue
		}

		currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if currentSize.Cmp(result.TargetSize) >= 0 {
			// Already at or above target
			continue
		}

		contextLogger.Info("Patching PVC for dynamic storage growth",
			"pvcName", pvc.Name,
			"currentSize", currentSize.String(),
			"targetSize", result.TargetSize.String())

		oldPVC := pvc.DeepCopy()
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = result.TargetSize
		if err := c.Patch(ctx, pvc, client.MergeFrom(oldPVC)); err != nil {
			return fmt.Errorf("error patching PVC %s: %w", pvc.Name, err)
		}
	}

	// Record the action
	if cluster.Status.StorageSizing != nil && cluster.Status.StorageSizing.Data != nil {
		status := cluster.Status.StorageSizing.Data
		status.LastAction = &apiv1.SizingAction{
			Kind:      string(result.Action),
			From:      result.CurrentSize.String(),
			To:        result.TargetSize.String(),
			Timestamp: metav1.Now(),
			Instance:  result.InstanceName,
			Result:    "Success",
		}
		status.EffectiveSize = result.TargetSize.String()
		status.Budget = IncrementBudgetUsage(&cluster.Spec.StorageConfiguration, status)
	}

	return nil
}

// GetEffectiveSizeForNewPVC returns the effective size to use for a new PVC.
// If dynamic sizing is enabled and we have an effective size from status, use that.
// Otherwise, use the request size.
func GetEffectiveSizeForNewPVC(cluster *apiv1.Cluster, volumeType VolumeType, tbsName string) string {
	var cfg *apiv1.StorageConfiguration
	var status *apiv1.VolumeSizingStatus

	switch volumeType {
	case VolumeTypeData:
		cfg = &cluster.Spec.StorageConfiguration
		if cluster.Status.StorageSizing != nil {
			status = cluster.Status.StorageSizing.Data
		}
	case VolumeTypeWAL:
		cfg = cluster.Spec.WalStorage
		if cluster.Status.StorageSizing != nil {
			status = cluster.Status.StorageSizing.WAL
		}
	case VolumeTypeTablespace:
		for i := range cluster.Spec.Tablespaces {
			if cluster.Spec.Tablespaces[i].Name == tbsName {
				cfg = &cluster.Spec.Tablespaces[i].Storage
				break
			}
		}
		if cluster.Status.StorageSizing != nil && cluster.Status.StorageSizing.Tablespaces != nil {
			status = cluster.Status.StorageSizing.Tablespaces[tbsName]
		}
	}

	if !IsDynamicSizingEnabled(cfg) {
		if cfg == nil {
			return ""
		}
		return cfg.Size
	}

	// Check if we have an effective size from previous growth
	if status != nil && status.EffectiveSize != "" {
		return status.EffectiveSize
	}

	// Use request as initial size
	return cfg.Request
}
