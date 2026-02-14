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
	"github.com/cloudnative-pg/machinery/pkg/log"
	"k8s.io/apimachinery/pkg/api/resource"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
)

const (
	// DefaultTargetBuffer is the default percentage of free space to maintain.
	DefaultTargetBuffer = 20

	// DefaultCriticalThreshold is the default usage percentage that triggers emergency growth.
	DefaultCriticalThreshold = 95

	// DefaultCriticalMinimumFree is the default minimum free space that triggers emergency growth.
	DefaultCriticalMinimumFree = "1Gi"

	// GrowthStepPercent is the percentage of current size to add during growth.
	GrowthStepPercent = 25
)

// IsDynamicSizingEnabled returns true if dynamic sizing is configured for the storage.
func IsDynamicSizingEnabled(cfg *apiv1.StorageConfiguration) bool {
	if cfg == nil {
		return false
	}
	return cfg.Request != "" && cfg.Limit != ""
}

// GetTargetBuffer returns the target buffer percentage from config or default.
func GetTargetBuffer(cfg *apiv1.StorageConfiguration) int {
	if cfg == nil || cfg.TargetBuffer == nil {
		return DefaultTargetBuffer
	}
	return *cfg.TargetBuffer
}

// GetCriticalThreshold returns the critical threshold percentage from config or default.
// Returns DefaultCriticalThreshold (95) when not configured or when set to 0 (Go zero value).
func GetCriticalThreshold(cfg *apiv1.StorageConfiguration) int {
	if cfg == nil || cfg.EmergencyGrow == nil {
		return DefaultCriticalThreshold
	}
	// CriticalThreshold is an int, so 0 is the zero value when field is not set.
	// We must return the default in this case to avoid triggering emergency growth
	// at any disk usage level.
	if cfg.EmergencyGrow.CriticalThreshold == 0 {
		return DefaultCriticalThreshold
	}
	return cfg.EmergencyGrow.CriticalThreshold
}

// GetCriticalMinimumFree returns the critical minimum free bytes from config or default.
func GetCriticalMinimumFree(cfg *apiv1.StorageConfiguration) resource.Quantity {
	if cfg == nil || cfg.EmergencyGrow == nil || cfg.EmergencyGrow.CriticalMinimumFree == "" {
		return resource.MustParse(DefaultCriticalMinimumFree)
	}
	qty, err := resource.ParseQuantity(cfg.EmergencyGrow.CriticalMinimumFree)
	if err != nil {
		log.Warning("Failed to parse criticalMinimumFree quantity, falling back to default",
			"criticalMinimumFree", cfg.EmergencyGrow.CriticalMinimumFree,
			"default", DefaultCriticalMinimumFree,
			"error", err)
		return resource.MustParse(DefaultCriticalMinimumFree)
	}
	return qty
}

// IsEmergencyGrowEnabled returns true if emergency growth is enabled.
func IsEmergencyGrowEnabled(cfg *apiv1.StorageConfiguration) bool {
	if cfg == nil || cfg.EmergencyGrow == nil {
		return true // default enabled
	}
	if cfg.EmergencyGrow.Enabled == nil {
		return true
	}
	return *cfg.EmergencyGrow.Enabled
}

// CalculateTargetSize calculates the ideal storage size based on used bytes and target buffer.
// Formula: targetSize = usedBytes / (1 - targetBuffer%)
func CalculateTargetSize(usedBytes uint64, targetBuffer int) resource.Quantity {
	// Align with API validation bounds (5-50%)
	if targetBuffer < 5 || targetBuffer > 50 {
		targetBuffer = DefaultTargetBuffer
	}

	// Calculate target: used / (1 - buffer%)
	bufferMultiplier := float64(100-targetBuffer) / 100.0
	targetBytes := float64(usedBytes) / bufferMultiplier

	// Round up to nearest Gi for cleaner sizes
	targetGi := int64(targetBytes / (1024 * 1024 * 1024))
	if targetBytes > float64(targetGi*1024*1024*1024) {
		targetGi++
	}
	if targetGi < 1 {
		targetGi = 1
	}

	return *resource.NewQuantity(targetGi*1024*1024*1024, resource.BinarySI)
}

// CalculateEmergencyGrowthSize calculates the new size for emergency growth.
// It grows by GrowthStepPercent of current size, up to the limit.
func CalculateEmergencyGrowthSize(currentSize, limit resource.Quantity) resource.Quantity {
	currentBytes := currentSize.Value()
	limitBytes := limit.Value()

	// Grow by 25% of current size
	growthBytes := currentBytes * GrowthStepPercent / 100

	// Minimum growth of 1Gi
	minGrowth := int64(1024 * 1024 * 1024)
	if growthBytes < minGrowth {
		growthBytes = minGrowth
	}

	newBytes := currentBytes + growthBytes
	if newBytes > limitBytes {
		newBytes = limitBytes
	}

	return *resource.NewQuantity(newBytes, resource.BinarySI)
}

// ClampSize clamps the target size between request and limit.
func ClampSize(target, request, limit resource.Quantity) resource.Quantity {
	if target.Cmp(request) < 0 {
		return request
	}
	if target.Cmp(limit) > 0 {
		return limit
	}
	return target
}

// IsEmergencyCondition checks if the current disk status triggers emergency growth.
func IsEmergencyCondition(cfg *apiv1.StorageConfiguration, totalBytes, usedBytes, availableBytes uint64) bool {
	if !IsEmergencyGrowEnabled(cfg) {
		return false
	}

	// Check percentage threshold
	threshold := GetCriticalThreshold(cfg)
	if totalBytes > 0 {
		percentUsed := float64(usedBytes) / float64(totalBytes) * 100
		if percentUsed >= float64(threshold) {
			return true
		}
	}

	// Check absolute minimum free
	criticalFree := GetCriticalMinimumFree(cfg)
	if int64(availableBytes) <= criticalFree.Value() { //nolint:gosec
		return true
	}

	return false
}

// NeedsGrowth checks if growth is needed based on current usage vs target buffer.
func NeedsGrowth(cfg *apiv1.StorageConfiguration, totalBytes, usedBytes uint64) bool {
	if totalBytes == 0 {
		return false
	}

	targetBuffer := GetTargetBuffer(cfg)
	currentFreePercent := float64(totalBytes-usedBytes) / float64(totalBytes) * 100

	// Need growth if free space is below target buffer
	return currentFreePercent < float64(targetBuffer)
}
