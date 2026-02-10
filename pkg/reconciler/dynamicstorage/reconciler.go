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

// VolumeState represents the state of a volume in StorageSizing status.
type VolumeState string

const (
	// VolumeStateBalanced indicates the volume is within target buffer.
	VolumeStateBalanced VolumeState = "Balanced"

	// VolumeStateNeedsGrow indicates the volume is below target buffer but not emergency.
	VolumeStateNeedsGrow VolumeState = "NeedsGrow"

	// VolumeStateEmergency indicates the volume is in emergency growth condition.
	VolumeStateEmergency VolumeState = "Emergency"

	// VolumeStatePendingGrowth indicates growth is queued waiting for window.
	VolumeStatePendingGrowth VolumeState = "PendingGrowth"

	// VolumeStateResizing indicates the volume is currently being resized by the provider.
	VolumeStateResizing VolumeState = "Resizing"

	// VolumeStateAtLimit indicates the volume has reached its configured limit.
	VolumeStateAtLimit VolumeState = "AtLimit"
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
	Action         ActionType
	TargetSize     resource.Quantity
	CurrentSize    resource.Quantity
	Reason         string
	VolumeType     VolumeType
	InstanceName   string
	TablespaceName string
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

	// Log input state for debugging
	instanceCount := 0
	if instanceStatuses != nil {
		instanceCount = len(instanceStatuses.Items)
	}
	contextLogger.Debug("Dynamic storage reconciler called",
		"instanceStatusCount", instanceCount,
		"pvcCount", len(pvcs))

	// Collect disk status from instance statuses
	diskStatusMap := collectDiskStatusForVolume(instanceStatuses, VolumeTypeData, "")
	contextLogger.Debug("Collected disk status map", "count", len(diskStatusMap))
	for podName, info := range diskStatusMap {
		contextLogger.Info("Instance disk status received",
			"pod", podName,
			"usedBytes", info.UsedBytes,
			"totalBytes", info.TotalBytes,
			"percentUsed", info.PercentUsed)
	}

	// Initialize storage sizing status if needed (do this early so status is always set)
	if cluster.Status.StorageSizing == nil {
		cluster.Status.StorageSizing = &apiv1.StorageSizingStatus{}
	}
	if cluster.Status.StorageSizing.Data == nil {
		cluster.Status.StorageSizing.Data = &apiv1.VolumeSizingStatus{}
	}

	if len(diskStatusMap) == 0 {
		// If we have instance statuses but no disk status yet, they might still be initializing.
		// Set status to indicate we're waiting, then return normally to allow status persistence.
		if instanceStatuses != nil && len(instanceStatuses.Items) > 0 {
			// Log which instances don't have disk status
			for _, status := range instanceStatuses.Items {
				podName := "unknown"
				if status.Pod != nil {
					podName = status.Pod.Name
				}
				hasDiskStatus := status.DiskStatus != nil
				hasError := status.Error != nil || status.ErrorMessage != ""
				contextLogger.Info("Instance disk status detail",
					"pod", podName,
					"hasDiskStatus", hasDiskStatus,
					"hasError", hasError,
					"errorMessage", status.ErrorMessage)
			}

			// Set status to indicate we're waiting for disk status
			// This allows users to see what's happening instead of silent failures
			cluster.Status.StorageSizing.Data.State = "WaitingForDiskStatus"

			contextLogger.Info("No disk status available yet, status updated to reflect waiting state",
				"instanceCount", len(instanceStatuses.Items))

			// Return normally (not requeue) to allow status to be persisted.
			// Normal reconciliation will retry when instance status updates.
			return ctrl.Result{}, nil
		}
		// No instances at all, nothing to do
		contextLogger.Debug("No instances available for disk status collection")
		return ctrl.Result{}, nil
	}

	// Evaluate sizing for data volume
	result := evaluateSizing(cluster, &cluster.Spec.StorageConfiguration, VolumeTypeData, "", diskStatusMap)

	// Update status based on result (status structures already initialized above)
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

	// Reconcile tablespaces
	if err := reconcileTablespaces(ctx, c, cluster, instanceStatuses, pvcs); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileTablespaces performs dynamic storage reconciliation for all tablespaces.
func reconcileTablespaces(
	ctx context.Context,
	c client.Client,
	cluster *apiv1.Cluster,
	instanceStatuses *postgres.PostgresqlStatusList,
	pvcs []corev1.PersistentVolumeClaim,
) error {
	contextLogger := log.FromContext(ctx)

	for i := range cluster.Spec.Tablespaces {
		tbs := &cluster.Spec.Tablespaces[i]
		if !IsDynamicSizingEnabled(&tbs.Storage) {
			continue
		}

		diskStatusMap := collectDiskStatusForVolume(instanceStatuses, VolumeTypeTablespace, tbs.Name)
		if len(diskStatusMap) == 0 {
			continue
		}

		result := evaluateSizing(cluster, &tbs.Storage, VolumeTypeTablespace, tbs.Name, diskStatusMap)
		result.TablespaceName = tbs.Name

		// Initialize status
		if cluster.Status.StorageSizing.Tablespaces == nil {
			cluster.Status.StorageSizing.Tablespaces = make(map[string]*apiv1.VolumeSizingStatus)
		}
		if cluster.Status.StorageSizing.Tablespaces[tbs.Name] == nil {
			cluster.Status.StorageSizing.Tablespaces[tbs.Name] = &apiv1.VolumeSizingStatus{}
		}

		updateVolumeStatus(cluster.Status.StorageSizing.Tablespaces[tbs.Name], &tbs.Storage, result)

		if result.Action != ActionNoOp && result.Action != ActionPendingGrowth {
			if err := executeAction(ctx, c, cluster, pvcs, result); err != nil {
				return fmt.Errorf("while executing tablespace %s dynamic storage action: %w", tbs.Name, err)
			}

			contextLogger.Info("Tablespace dynamic storage action executed",
				"tablespace", tbs.Name,
				"action", result.Action,
				"currentSize", result.CurrentSize.String(),
				"targetSize", result.TargetSize.String())
		}
	}

	return nil
}

// collectDiskStatusForVolume collects disk status for a specific volume type from all instance statuses.
func collectDiskStatusForVolume(
	statuses *postgres.PostgresqlStatusList,
	volumeType VolumeType,
	tbsName string,
) map[string]*DiskInfo {
	if statuses == nil {
		return nil
	}

	result := make(map[string]*DiskInfo)
	for _, status := range statuses.Items {
		if status.Pod == nil {
			continue
		}

		var ds *postgres.DiskStatus
		switch volumeType {
		case VolumeTypeData:
			ds = status.DiskStatus
		case VolumeTypeWAL:
			ds = status.WALDiskStatus
		case VolumeTypeTablespace:
			if status.TablespaceDiskStatus != nil {
				ds = status.TablespaceDiskStatus[tbsName]
			}
		}

		if ds == nil {
			continue
		}

		result[status.Pod.Name] = &DiskInfo{
			TotalBytes:     ds.TotalBytes,
			UsedBytes:      ds.UsedBytes,
			AvailableBytes: ds.AvailableBytes,
			PercentUsed:    ds.PercentUsed,
		}
	}
	return result
}

// evaluateSizing evaluates the sizing needs for a volume type.
func evaluateSizing(
	cluster *apiv1.Cluster,
	cfg *apiv1.StorageConfiguration,
	volumeType VolumeType,
	tbsName string,
	diskStatusMap map[string]*DiskInfo,
) *ReconcileResult {
	maxUsed, maxTotal, minAvailable, highestUsageInstance := findMaxUsage(diskStatusMap)

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

	volumeStatus := getVolumeSizingStatus(cluster, volumeType, tbsName)

	// Check for emergency condition
	if IsEmergencyCondition(cfg, maxTotal, maxUsed, minAvailable) {
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

	return evaluateGrowth(cfg, volumeStatus, volumeType, currentSize, request, limit, maxTotal, maxUsed, highestUsageInstance)
}

func findMaxUsage(diskStatusMap map[string]*DiskInfo) (uint64, uint64, uint64, string) {
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
	return maxUsed, maxTotal, minAvailable, highestUsageInstance
}

func getVolumeSizingStatus(cluster *apiv1.Cluster, volumeType VolumeType, tbsName string) *apiv1.VolumeSizingStatus {
	if cluster.Status.StorageSizing == nil {
		return nil
	}
	switch volumeType {
	case VolumeTypeData:
		return cluster.Status.StorageSizing.Data
	case VolumeTypeWAL:
		return cluster.Status.StorageSizing.WAL
	case VolumeTypeTablespace:
		if cluster.Status.StorageSizing.Tablespaces != nil {
			return cluster.Status.StorageSizing.Tablespaces[tbsName]
		}
	}
	return nil
}

func evaluateGrowth(
	cfg *apiv1.StorageConfiguration,
	volumeStatus *apiv1.VolumeSizingStatus,
	volumeType VolumeType,
	currentSize, request, limit resource.Quantity,
	maxTotal, maxUsed uint64,
	highestUsageInstance string,
) *ReconcileResult {
	// Check if growth is needed
	if !NeedsGrowth(cfg, maxTotal, maxUsed) {
		return &ReconcileResult{
			Action:      ActionNoOp,
			VolumeType:  volumeType,
			CurrentSize: currentSize,
			Reason:      "balanced",
		}
	}

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
			status.State = string(VolumeStateAtLimit)
		} else {
			status.State = string(VolumeStateBalanced)
		}
	case ActionEmergencyGrow:
		status.State = string(VolumeStateEmergency)
	case ActionScheduledGrow:
		status.State = string(VolumeStateResizing)
	case ActionPendingGrowth:
		status.State = string(VolumeStatePendingGrowth)
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

	// Find relevant PVCs and patch them
	for i := range pvcs {
		pvc := &pvcs[i]

		// Check if this PVC belongs to the volume type we're reconciling
		role := pvc.Labels[utils.PvcRoleLabelName]
		switch result.VolumeType {
		case VolumeTypeData:
			if role != string(utils.PVCRolePgData) {
				continue
			}
		case VolumeTypeWAL:
			if role != string(utils.PVCRolePgWal) {
				continue
			}
		case VolumeTypeTablespace:
			if role != string(utils.PVCRolePgTablespace) {
				continue
			}
			tbsName := pvc.Labels[utils.TablespaceNameLabelName]
			if tbsName != result.TablespaceName {
				continue
			}
		}

		currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if currentSize.Cmp(result.TargetSize) >= 0 {
			// Already at or above target
			continue
		}

		contextLogger.Info("Patching PVC for dynamic storage growth",
			"pvcName", pvc.Name,
			"volumeType", result.VolumeType,
			"currentSize", currentSize.String(),
			"targetSize", result.TargetSize.String())

		oldPVC := pvc.DeepCopy()
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = result.TargetSize
		if err := c.Patch(ctx, pvc, client.MergeFrom(oldPVC)); err != nil {
			return fmt.Errorf("error patching PVC %s: %w", pvc.Name, err)
		}
	}

	// Record the action in status
	var status *apiv1.VolumeSizingStatus
	var cfg *apiv1.StorageConfiguration

	if cluster.Status.StorageSizing != nil {
		switch result.VolumeType {
		case VolumeTypeData:
			status = cluster.Status.StorageSizing.Data
			cfg = &cluster.Spec.StorageConfiguration
		case VolumeTypeWAL:
			status = cluster.Status.StorageSizing.WAL
			cfg = cluster.Spec.WalStorage
		case VolumeTypeTablespace:
			tbsName := result.TablespaceName
			if cluster.Status.StorageSizing.Tablespaces != nil {
				status = cluster.Status.StorageSizing.Tablespaces[tbsName]
			}
			for i := range cluster.Spec.Tablespaces {
				if cluster.Spec.Tablespaces[i].Name == tbsName {
					cfg = &cluster.Spec.Tablespaces[i].Storage
					break
				}
			}
		}
	}

	if status != nil && cfg != nil {
		status.LastAction = &apiv1.SizingAction{
			Kind:      string(result.Action),
			From:      result.CurrentSize.String(),
			To:        result.TargetSize.String(),
			Timestamp: metav1.Now(),
			Instance:  result.InstanceName,
			Result:    "Success",
		}
		status.EffectiveSize = result.TargetSize.String()
		status.Budget = IncrementBudgetUsage(cfg, status)
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
