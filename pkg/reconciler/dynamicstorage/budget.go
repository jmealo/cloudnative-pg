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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
)

const (
	// DefaultMaxActionsPerDay is the default maximum resize operations per 24 hours.
	DefaultMaxActionsPerDay = 4

	// DefaultReservedForEmergency is the default number of actions reserved for emergency.
	DefaultReservedForEmergency = 1
)

// GetMaxActionsPerDay returns the max actions per day from config or default.
func GetMaxActionsPerDay(cfg *apiv1.StorageConfiguration) int {
	if cfg == nil || cfg.EmergencyGrow == nil || cfg.EmergencyGrow.MaxActionsPerDay == nil {
		return DefaultMaxActionsPerDay
	}
	return *cfg.EmergencyGrow.MaxActionsPerDay
}

// GetReservedForEmergency returns the number of actions reserved for emergency from config or default.
func GetReservedForEmergency(cfg *apiv1.StorageConfiguration) int {
	if cfg == nil || cfg.EmergencyGrow == nil || cfg.EmergencyGrow.ReservedActionsForEmergency == nil {
		return DefaultReservedForEmergency
	}
	return *cfg.EmergencyGrow.ReservedActionsForEmergency
}

// HasBudgetForEmergency checks if there's budget available for an emergency resize.
func HasBudgetForEmergency(cfg *apiv1.StorageConfiguration, status *apiv1.VolumeSizingStatus) bool {
	if status == nil || status.Budget == nil {
		return true // No budget tracking = unlimited
	}

	maxActions := GetMaxActionsPerDay(cfg)
	actionsUsed := status.Budget.ActionsLast24h

	// Emergency can use the full budget
	return actionsUsed < maxActions
}

// HasBudgetForScheduled checks if there's budget available for a scheduled resize.
func HasBudgetForScheduled(_ *apiv1.StorageConfiguration, status *apiv1.VolumeSizingStatus) bool {
	if status == nil || status.Budget == nil {
		return true // No budget tracking = unlimited
	}

	return status.Budget.AvailableForPlanned > 0
}

// CalculateBudget calculates the current budget state based on last action and config.
func CalculateBudget(cfg *apiv1.StorageConfiguration, status *apiv1.VolumeSizingStatus) *apiv1.BudgetStatus {
	maxActions := GetMaxActionsPerDay(cfg)
	reserved := GetReservedForEmergency(cfg)

	// Count actions in last 24 hours
	actionsLast24h := 0
	if status != nil && status.LastAction != nil {
		// Check if last action was within 24 hours
		if time.Since(status.LastAction.Timestamp.Time) < 24*time.Hour {
			if status.Budget != nil {
				actionsLast24h = status.Budget.ActionsLast24h
			}
		}
	}

	availableTotal := maxActions - actionsLast24h
	if availableTotal < 0 {
		availableTotal = 0
	}

	availableForEmergency := min(reserved, availableTotal)
	availableForPlanned := availableTotal - availableForEmergency
	if availableForPlanned < 0 {
		availableForPlanned = 0
	}

	// Calculate when budget resets (24h from oldest tracked action)
	budgetResetsAt := metav1.NewTime(time.Now().Add(24 * time.Hour))

	return &apiv1.BudgetStatus{
		ActionsLast24h:        actionsLast24h,
		AvailableForPlanned:   availableForPlanned,
		AvailableForEmergency: availableForEmergency,
		BudgetResetsAt:        budgetResetsAt,
	}
}

// IncrementBudgetUsage increments the action count and returns updated budget.
func IncrementBudgetUsage(cfg *apiv1.StorageConfiguration, status *apiv1.VolumeSizingStatus) *apiv1.BudgetStatus {
	budget := CalculateBudget(cfg, status)
	budget.ActionsLast24h++

	// Recalculate available actions
	maxActions := GetMaxActionsPerDay(cfg)
	reserved := GetReservedForEmergency(cfg)

	availableTotal := maxActions - budget.ActionsLast24h
	if availableTotal < 0 {
		availableTotal = 0
	}

	budget.AvailableForEmergency = min(reserved, availableTotal)
	budget.AvailableForPlanned = availableTotal - budget.AvailableForEmergency
	if budget.AvailableForPlanned < 0 {
		budget.AvailableForPlanned = 0
	}

	return budget
}
