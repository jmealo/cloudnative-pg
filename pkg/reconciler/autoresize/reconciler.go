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

package autoresize

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudnative-pg/machinery/pkg/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
)

// Reconciler handles auto-resize logic for PVCs
type Reconciler struct {
	client   client.Client
	recorder record.EventRecorder
}

// NewReconciler creates a new auto-resize reconciler
func NewReconciler(c client.Client, recorder record.EventRecorder) *Reconciler {
	return &Reconciler{
		client:   c,
		recorder: recorder,
	}
}

// Reconcile evaluates and performs auto-resize operations for all volumes
// that have auto-resize enabled and exceed their configured thresholds
func (r *Reconciler) Reconcile(
	ctx context.Context,
	cluster *apiv1.Cluster,
	diskStatuses []apiv1.InstanceDiskStatus,
	pvcs []corev1.PersistentVolumeClaim,
) (ctrl.Result, error) {
	contextLogger := log.FromContext(ctx)

	// Skip if no auto-resize configured anywhere
	if !r.hasAutoResizeEnabled(cluster) {
		return ctrl.Result{}, nil
	}

	// Process each instance
	for i := range diskStatuses {
		status := &diskStatuses[i]

		// Check data volume
		if cluster.Spec.StorageConfiguration.AutoResize != nil &&
			cluster.Spec.StorageConfiguration.AutoResize.Enabled {
			if err := r.reconcileVolume(ctx, cluster, status, "data",
				cluster.Spec.StorageConfiguration.AutoResize,
				status.Data,
				r.findPVC(pvcs, status.PodName),
			); err != nil {
				contextLogger.Error(err, "Failed to reconcile data volume auto-resize",
					"pod", status.PodName)
				return ctrl.Result{}, err
			}
		}

		// Check WAL volume (if separate)
		if cluster.ShouldCreateWalArchiveVolume() &&
			cluster.Spec.WalStorage != nil &&
			cluster.Spec.WalStorage.AutoResize != nil &&
			cluster.Spec.WalStorage.AutoResize.Enabled {
			if err := r.reconcileVolume(ctx, cluster, status, "wal",
				cluster.Spec.WalStorage.AutoResize,
				status.WAL,
				r.findPVC(pvcs, status.PodName+"-wal"),
			); err != nil {
				contextLogger.Error(err, "Failed to reconcile WAL volume auto-resize",
					"pod", status.PodName)
				return ctrl.Result{}, err
			}
		}

		// Check tablespaces
		for _, ts := range cluster.Spec.Tablespaces {
			if ts.Storage.AutoResize != nil && ts.Storage.AutoResize.Enabled {
				tbsStatus := status.Tablespaces[ts.Name]
				if err := r.reconcileVolume(ctx, cluster, status, "tablespace:"+ts.Name,
					ts.Storage.AutoResize,
					tbsStatus,
					r.findPVC(pvcs, status.PodName+"-tbs-"+ts.Name),
				); err != nil {
					contextLogger.Error(err, "Failed to reconcile tablespace volume auto-resize",
						"pod", status.PodName, "tablespace", ts.Name)
					return ctrl.Result{}, err
				}
			}
		}
	}

	// Requeue to continue monitoring
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// hasAutoResizeEnabled checks if any volume has auto-resize enabled
func (r *Reconciler) hasAutoResizeEnabled(cluster *apiv1.Cluster) bool {
	if cluster.Spec.StorageConfiguration.AutoResize != nil &&
		cluster.Spec.StorageConfiguration.AutoResize.Enabled {
		return true
	}
	if cluster.Spec.WalStorage != nil &&
		cluster.Spec.WalStorage.AutoResize != nil &&
		cluster.Spec.WalStorage.AutoResize.Enabled {
		return true
	}
	for _, ts := range cluster.Spec.Tablespaces {
		if ts.Storage.AutoResize != nil && ts.Storage.AutoResize.Enabled {
			return true
		}
	}
	return false
}

// reconcileVolume handles auto-resize for a single volume
func (r *Reconciler) reconcileVolume(
	ctx context.Context,
	cluster *apiv1.Cluster,
	instanceStatus *apiv1.InstanceDiskStatus,
	volumeType string,
	config *apiv1.AutoResizeConfiguration,
	volumeStatus *apiv1.VolumeDiskStatus,
	pvc *corev1.PersistentVolumeClaim,
) error {
	contextLogger := log.FromContext(ctx)

	if config == nil || !config.Enabled || volumeStatus == nil || pvc == nil {
		return nil
	}

	// Check if threshold exceeded
	if volumeStatus.PercentUsed < config.Threshold {
		return nil
	}

	contextLogger.Info("Volume usage exceeds threshold",
		"pvc", pvc.Name,
		"volumeType", volumeType,
		"percentUsed", volumeStatus.PercentUsed,
		"threshold", config.Threshold)

	// Check cooldown
	if !r.cooldownExpired(cluster, pvc.Name, config.CooldownPeriod) {
		contextLogger.Info("Auto-resize in cooldown period", "pvc", pvc.Name)
		return nil
	}

	// Check max size
	if config.MaxSize != "" {
		maxSize := resource.MustParse(config.MaxSize)
		currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if currentSize.Cmp(maxSize) >= 0 {
			if r.recorder != nil {
				r.recorder.Eventf(cluster, corev1.EventTypeWarning, "AutoResizeMaxReached",
					"PVC %s has reached max size %s", pvc.Name, config.MaxSize)
			}
			contextLogger.Info("PVC has reached maximum size",
				"pvc", pvc.Name, "maxSize", config.MaxSize)
			return nil
		}
	}

	// WAL safety checks for WAL volume or single-volume data
	isSingleVolume := !cluster.ShouldCreateWalArchiveVolume()
	if volumeType == "wal" || (volumeType == "data" && isSingleVolume) {
		safe, reason := r.evaluateWALSafety(instanceStatus.WALHealth, config.WALSafetyPolicy)
		if !safe {
			if r.recorder != nil {
				r.recorder.Eventf(cluster, corev1.EventTypeWarning, "AutoResizeBlocked",
					"Auto-resize blocked for %s: %s", pvc.Name, reason)
			}
			// Update condition
			meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
				Type:    "AutoResizeBlocked",
				Status:  metav1.ConditionTrue,
				Reason:  "WALHealthCheckFailed",
				Message: fmt.Sprintf("PVC %s: %s", pvc.Name, reason),
			})
			contextLogger.Info("Auto-resize blocked by WAL safety check",
				"pvc", pvc.Name, "reason", reason)
			return nil
		}
	}

	// Calculate new size
	newSize, err := r.calculateNewSize(pvc, config)
	if err != nil {
		return err
	}

	oldSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]

	// Perform resize
	if err := r.resizePVC(ctx, pvc, newSize); err != nil {
		if r.recorder != nil {
			r.recorder.Eventf(cluster, corev1.EventTypeWarning, "AutoResizeFailed",
				"Failed to resize %s: %v", pvc.Name, err)
		}
		return err
	}

	// Record event
	if r.recorder != nil {
		r.recorder.Eventf(cluster, corev1.EventTypeNormal, "AutoResizeSucceeded",
			"Resized %s from %s to %s (usage was %d%%)",
			pvc.Name, oldSize.String(), newSize.String(), volumeStatus.PercentUsed)
	}

	contextLogger.Info("Successfully resized PVC",
		"pvc", pvc.Name,
		"oldSize", oldSize.String(),
		"newSize", newSize.String(),
		"percentUsed", volumeStatus.PercentUsed)

	// Update status
	if cluster.Status.DiskStatus == nil {
		cluster.Status.DiskStatus = &apiv1.ClusterDiskStatus{}
	}
	cluster.Status.DiskStatus.LastAutoResize = &apiv1.AutoResizeEvent{
		Time:       metav1.Now(),
		PodName:    instanceStatus.PodName,
		PVCName:    pvc.Name,
		VolumeType: volumeType,
		OldSize:    oldSize.String(),
		NewSize:    newSize.String(),
		Reason: fmt.Sprintf("Usage exceeded threshold: %d%% > %d%%",
			volumeStatus.PercentUsed, config.Threshold),
	}

	// Clear blocked condition if it was set
	meta.RemoveStatusCondition(&cluster.Status.Conditions, "AutoResizeBlocked")

	return nil
}

// evaluateWALSafety checks if WAL health allows safe resize
func (r *Reconciler) evaluateWALSafety(
	health *apiv1.WALHealthInfo,
	policy *apiv1.WALSafetyPolicy,
) (bool, string) {
	if policy == nil {
		return true, ""
	}

	// Check archive health
	requireHealthy := policy.RequireArchiveHealthy == nil || *policy.RequireArchiveHealthy
	if requireHealthy && health != nil && !health.ArchiveHealthy {
		return false, fmt.Sprintf("WAL archive unhealthy: %d files pending",
			health.PendingArchiveFiles)
	}

	// Check pending file count
	maxPending := 100
	if policy.MaxPendingWALFiles != nil {
		maxPending = *policy.MaxPendingWALFiles
	}
	if maxPending > 0 && health != nil && health.PendingArchiveFiles > maxPending {
		return false, fmt.Sprintf("Too many pending WAL files: %d > %d",
			health.PendingArchiveFiles, maxPending)
	}

	// Check inactive slots
	if policy.MaxSlotRetentionBytes != nil && health != nil {
		if len(health.InactiveReplicationSlots) > 0 {
			return false, fmt.Sprintf("Inactive replication slots detected: %v",
				health.InactiveReplicationSlots)
		}
	}

	return true, ""
}

// calculateNewSize computes the new PVC size based on the increase configuration
func (r *Reconciler) calculateNewSize(
	pvc *corev1.PersistentVolumeClaim,
	config *apiv1.AutoResizeConfiguration,
) (resource.Quantity, error) {
	currentSize := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	increase := config.Increase

	if increase == "" {
		increase = "20%"
	}

	var newSize resource.Quantity

	if strings.HasSuffix(increase, "%") {
		// Percentage increase
		pct, err := strconv.ParseFloat(strings.TrimSuffix(increase, "%"), 64)
		if err != nil {
			return newSize, err
		}
		currentBytes := currentSize.Value()
		increaseBytes := int64(float64(currentBytes) * pct / 100)
		newSize = *resource.NewQuantity(currentBytes+increaseBytes, resource.BinarySI)
	} else {
		// Absolute increase
		increaseQty, err := resource.ParseQuantity(increase)
		if err != nil {
			return newSize, err
		}
		currentBytes := currentSize.Value()
		newSize = *resource.NewQuantity(currentBytes+increaseQty.Value(), resource.BinarySI)
	}

	// Cap at maxSize
	if config.MaxSize != "" {
		maxSize := resource.MustParse(config.MaxSize)
		if newSize.Cmp(maxSize) > 0 {
			newSize = maxSize
		}
	}

	return newSize, nil
}

// cooldownExpired checks if enough time has passed since the last resize
func (r *Reconciler) cooldownExpired(
	cluster *apiv1.Cluster,
	pvcName string,
	cooldownPeriod *metav1.Duration,
) bool {
	if cluster.Status.DiskStatus == nil || cluster.Status.DiskStatus.LastAutoResize == nil {
		return true
	}

	lastResize := cluster.Status.DiskStatus.LastAutoResize
	if lastResize.PVCName != pvcName {
		return true
	}

	cooldown := time.Hour // default
	if cooldownPeriod != nil {
		cooldown = cooldownPeriod.Duration
	}

	return time.Since(lastResize.Time.Time) >= cooldown
}

// resizePVC updates the PVC with the new size
func (r *Reconciler) resizePVC(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	newSize resource.Quantity,
) error {
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = newSize
	return r.client.Update(ctx, pvc)
}

// findPVC finds a PVC by name in the list
func (r *Reconciler) findPVC(pvcs []corev1.PersistentVolumeClaim, name string) *corev1.PersistentVolumeClaim {
	for i := range pvcs {
		if pvcs[i].Name == name {
			return &pvcs[i]
		}
	}
	return nil
}
