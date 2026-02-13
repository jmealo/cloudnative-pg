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

			// Requeue to check again soon
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		// No instances at all, nothing to do yet. Return empty result to allow
		// the cluster controller to continue with instance creation.
		contextLogger.Debug("No instances available for disk status collection")
		return ctrl.Result{}, nil
	}

	// Collect actual PVC sizes (needed for both sizing evaluation and status)
	actualSizes := collectActualSizes(pvcs, VolumeTypeData, "")
	contextLogger.Debug("Collected actual PVC sizes",
		"count", len(actualSizes),
		"sizes", actualSizes,
		"pvcCount", len(pvcs))

	// Evaluate sizing for data volume, using PVC capacity as the authoritative current size.
	// Filesystem TotalBytes from statfs is smaller than PVC capacity due to filesystem
	// metadata overhead (~3% for ext4/xfs), so we must use PVC capacity to avoid
	// false growth actions (e.g., "growing" from 4.84Gi → 5Gi when PVC is already 5Gi).
	result := evaluateSizing(cluster, &cluster.Spec.StorageConfiguration, VolumeTypeData, "", diskStatusMap, actualSizes)

	// Update status based on result (status structures already initialized above)
	updateVolumeStatus(cluster.Status.StorageSizing.Data, &cluster.Spec.StorageConfiguration, result, actualSizes)

	// Execute action if needed (skip NoOp and PendingGrowth which don't modify PVCs)
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

	// Always persist status updates when the action indicates a state change.
	// PendingGrowth needs to be persisted so the test can observe the state,
	// and other actions need their status changes persisted as well.
	if result.Action != ActionNoOp {
		if err := c.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("while updating cluster status after dynamic storage action: %w", err)
		}
	}

	// Reconcile tablespaces
	res, err := reconcileTablespaces(ctx, c, cluster, instanceStatuses, pvcs)
	if err != nil || !res.IsZero() {
		return res, err
	}

	// Return empty result to allow the cluster controller to continue its reconciliation.
	return ctrl.Result{}, nil
}

// reconcileTablespaces performs dynamic storage reconciliation for all tablespaces.
func reconcileTablespaces(
	ctx context.Context,
	c client.Client,
	cluster *apiv1.Cluster,
	instanceStatuses *postgres.PostgresqlStatusList,
	pvcs []corev1.PersistentVolumeClaim,
) (ctrl.Result, error) {
	contextLogger := log.FromContext(ctx)

	for i := range cluster.Spec.Tablespaces {
		tbs := &cluster.Spec.Tablespaces[i]
		if !IsDynamicSizingEnabled(&tbs.Storage) {
			continue
		}

		diskStatusMap := collectDiskStatusForVolume(instanceStatuses, VolumeTypeTablespace, tbs.Name)
		if len(diskStatusMap) == 0 {
			// Initialize status for this tablespace so we can report why we're waiting
			if cluster.Status.StorageSizing.Tablespaces == nil {
				cluster.Status.StorageSizing.Tablespaces = make(map[string]*apiv1.VolumeSizingStatus)
			}
			if cluster.Status.StorageSizing.Tablespaces[tbs.Name] == nil {
				cluster.Status.StorageSizing.Tablespaces[tbs.Name] = &apiv1.VolumeSizingStatus{}
			}

			// Set state to indicate we're waiting for disk status
			if instanceStatuses != nil && len(instanceStatuses.Items) > 0 {
				cluster.Status.StorageSizing.Tablespaces[tbs.Name].State = "WaitingForDiskStatus"
				contextLogger.Info("No disk status available for tablespace yet, will retry on next reconciliation",
					"tablespace", tbs.Name,
					"instanceCount", len(instanceStatuses.Items))
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			continue
		}

		// Initialize status
		if cluster.Status.StorageSizing.Tablespaces == nil {
			cluster.Status.StorageSizing.Tablespaces = make(map[string]*apiv1.VolumeSizingStatus)
		}
		if cluster.Status.StorageSizing.Tablespaces[tbs.Name] == nil {
			cluster.Status.StorageSizing.Tablespaces[tbs.Name] = &apiv1.VolumeSizingStatus{}
		}

		// Collect actual PVC sizes for this tablespace
		actualSizes := collectActualSizes(pvcs, VolumeTypeTablespace, tbs.Name)

		result := evaluateSizing(cluster, &tbs.Storage, VolumeTypeTablespace, tbs.Name, diskStatusMap, actualSizes)
		result.TablespaceName = tbs.Name

		updateVolumeStatus(cluster.Status.StorageSizing.Tablespaces[tbs.Name], &tbs.Storage, result, actualSizes)

		if result.Action != ActionNoOp && result.Action != ActionPendingGrowth {
			if err := executeAction(ctx, c, cluster, pvcs, result); err != nil {
				return ctrl.Result{}, fmt.Errorf("while executing tablespace %s dynamic storage action: %w", tbs.Name, err)
			}

			contextLogger.Info("Tablespace dynamic storage action executed",
				"tablespace", tbs.Name,
				"action", result.Action,
				"currentSize", result.CurrentSize.String(),
				"targetSize", result.TargetSize.String())
		}

		// Always persist status updates when the action indicates a state change
		if result.Action != ActionNoOp {
			if err := c.Status().Update(ctx, cluster); err != nil {
				return ctrl.Result{}, fmt.Errorf("while updating cluster status after tablespace %s dynamic storage action: %w", tbs.Name, err)
			}
		}
	}

	return ctrl.Result{}, nil
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
// pvcSizes maps instance names to their PVC capacity strings (from Status.Capacity or Spec.Resources.Requests).
// We use PVC capacity as the authoritative "current size" because filesystem TotalBytes (from statfs)
// is reduced by filesystem metadata overhead (~3% for ext4/xfs), which would cause false growth actions.
func evaluateSizing(
	cluster *apiv1.Cluster,
	cfg *apiv1.StorageConfiguration,
	volumeType VolumeType,
	tbsName string,
	diskStatusMap map[string]*DiskInfo,
	pvcSizes map[string]string,
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

	// Use PVC capacity as currentSize rather than filesystem TotalBytes.
	// Filesystem metadata overhead (~3% for ext4/xfs) makes statfs.TotalBytes smaller
	// than the actual PVC capacity, which would cause false growth (e.g., "growing"
	// from 4.84Gi → 5Gi when PVC is already 5Gi).
	currentSize := maxPVCSize(pvcSizes)
	pvcSizesAvailable := !currentSize.IsZero()
	if currentSize.IsZero() {
		// Fallback to filesystem TotalBytes if no PVC sizes available.
		// This is only safe for emergency conditions where we must act regardless.
		// For scheduled growth, we need accurate PVC sizes to avoid false growth.
		currentSize = *resource.NewQuantity(int64(maxTotal), resource.BinarySI) //nolint:gosec
	}

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
		// Calculate target size even when budget exhausted for metrics/status
		targetSize := CalculateTargetSize(maxUsed, GetTargetBuffer(cfg))
		return &ReconcileResult{
			Action:      ActionNoOp,
			VolumeType:  volumeType,
			CurrentSize: currentSize,
			TargetSize:  targetSize,
			Reason:      "emergency budget exhausted",
		}
	}

	return evaluateGrowth(
		cfg, volumeStatus, volumeType,
		currentSize, request, limit,
		maxTotal, maxUsed, highestUsageInstance,
		pvcSizesAvailable,
	)
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

// maxPVCSize returns the largest PVC capacity from the provided map.
// This gives the authoritative current volume size, unaffected by filesystem overhead.
func maxPVCSize(pvcSizes map[string]string) resource.Quantity {
	var max resource.Quantity
	for _, sizeStr := range pvcSizes {
		qty, err := resource.ParseQuantity(sizeStr)
		if err != nil {
			continue
		}
		if qty.Cmp(max) > 0 {
			max = qty
		}
	}
	return max
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
	pvcSizesAvailable bool,
) *ReconcileResult {
	// Calculate target size first for consistent metrics/status reporting
	targetSize := CalculateTargetSize(maxUsed, GetTargetBuffer(cfg))
	targetSize = ClampSize(targetSize, request, limit)

	// Check if growth is needed
	if !NeedsGrowth(cfg, maxTotal, maxUsed) {
		return &ReconcileResult{
			Action:      ActionNoOp,
			VolumeType:  volumeType,
			CurrentSize: currentSize,
			TargetSize:  targetSize,
			Reason:      "balanced",
		}
	}

	// If growth is needed but we don't have accurate PVC sizes, we cannot safely
	// proceed with scheduled growth. The filesystem TotalBytes is smaller than PVC
	// capacity due to overhead, so target calculations would be inaccurate.
	// We return NoOp and wait for PVC size data to be available.
	if !pvcSizesAvailable {
		return &ReconcileResult{
			Action:      ActionNoOp,
			VolumeType:  volumeType,
			CurrentSize: currentSize,
			TargetSize:  targetSize,
			Reason:      "waiting for PVC size data",
		}
	}

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

// collectActualSizes collects the current PVC sizes for instances of a specific volume type.
func collectActualSizes(
	pvcs []corev1.PersistentVolumeClaim,
	volumeType VolumeType,
	tbsName string,
) map[string]string {
	actualSizes := make(map[string]string)

	for i := range pvcs {
		pvc := &pvcs[i]

		// Check if this PVC matches the volume type
		role := pvc.Labels[utils.PvcRoleLabelName]
		var matches bool
		switch volumeType {
		case VolumeTypeData:
			matches = role == string(utils.PVCRolePgData)
		case VolumeTypeWAL:
			matches = role == string(utils.PVCRolePgWal)
		case VolumeTypeTablespace:
			matches = role == string(utils.PVCRolePgTablespace) &&
				pvc.Labels[utils.TablespaceNameLabelName] == tbsName
		}

		if !matches {
			continue
		}

		// Get instance name from PVC labels
		instanceName := pvc.Labels[utils.InstanceNameLabelName]
		if instanceName == "" {
			continue
		}

		// Get the current size from PVC status (actual size) or spec (requested size)
		// Use status.capacity if available (actual provisioned size), otherwise use spec.resources.requests
		if capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			actualSizes[instanceName] = capacity.String()
		} else if size, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			actualSizes[instanceName] = size.String()
		}
	}

	return actualSizes
}

// updateVolumeStatus updates the volume sizing status based on the result.
func updateVolumeStatus(
	status *apiv1.VolumeSizingStatus,
	cfg *apiv1.StorageConfiguration,
	result *ReconcileResult,
	actualSizes map[string]string,
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

	// Update actual sizes
	if len(actualSizes) > 0 {
		status.ActualSizes = actualSizes
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

	patchedCount, err := patchPVCsForVolume(ctx, c, pvcs, result)
	if err != nil {
		return err
	}

	// If no PVCs were actually patched, this means the PVCs were already at or above
	// the target size. This can happen when result.CurrentSize was calculated from
	// filesystem TotalBytes (which has ~3% overhead) but PVC capacity is already
	// at the target. Convert to NoOp to avoid false "Success" reports.
	if patchedCount == 0 {
		contextLogger.Info("No PVCs needed patching - all already at target size",
			"action", result.Action,
			"targetSize", result.TargetSize.String(),
			"reason", "PVCs already at or above target size")
		// Don't update LastAction status since no actual growth happened
		return nil
	}

	return updateStatusAfterAction(cluster, result)
}

func patchPVCsForVolume(
	ctx context.Context,
	c client.Client,
	pvcs []corev1.PersistentVolumeClaim,
	result *ReconcileResult,
) (int, error) {
	contextLogger := log.FromContext(ctx)
	patchedCount := 0

	for i := range pvcs {
		pvc := &pvcs[i]
		if !isPVCForVolume(pvc, result) {
			continue
		}

		currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if currentSize.Cmp(result.TargetSize) >= 0 {
			// PVC already at or above target size - log this for debugging
			contextLogger.Info("Skipping PVC patch - already at target size",
				"pvcName", pvc.Name,
				"volumeType", result.VolumeType,
				"pvcCurrentSize", currentSize.String(),
				"targetSize", result.TargetSize.String(),
				"resultCurrentSize", result.CurrentSize.String())
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
			return patchedCount, fmt.Errorf("error patching PVC %s: %w", pvc.Name, err)
		}
		patchedCount++
	}
	return patchedCount, nil
}

func isPVCForVolume(pvc *corev1.PersistentVolumeClaim, result *ReconcileResult) bool {
	role := pvc.Labels[utils.PvcRoleLabelName]
	switch result.VolumeType {
	case VolumeTypeData:
		return role == string(utils.PVCRolePgData)
	case VolumeTypeWAL:
		return role == string(utils.PVCRolePgWal)
	case VolumeTypeTablespace:
		if role != string(utils.PVCRolePgTablespace) {
			return false
		}
		return pvc.Labels[utils.TablespaceNameLabelName] == result.TablespaceName
	}
	return false
}

func updateStatusAfterAction(cluster *apiv1.Cluster, result *ReconcileResult) error {
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
