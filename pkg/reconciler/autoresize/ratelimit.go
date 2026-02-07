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
	"sync"
	"time"
)

// BudgetTracker tracks the resize budget for each volume using a 24-hour rolling window
type BudgetTracker struct {
	mu         sync.Mutex
	timeStamps map[string][]time.Time
}

// NewBudgetTracker creates a new BudgetTracker instance
func NewBudgetTracker() *BudgetTracker {
	return &BudgetTracker{
		timeStamps: make(map[string][]time.Time),
	}
}

// cleanExpiredEvents removes timestamps older than 24 hours
func (bt *BudgetTracker) cleanExpiredEvents(volumeKey string) {
	cutoffTime := time.Now().Add(-24 * time.Hour)
	timestamps := bt.timeStamps[volumeKey]

	validTimestamps := make([]time.Time, 0, len(timestamps))
	for _, ts := range timestamps {
		if ts.After(cutoffTime) {
			validTimestamps = append(validTimestamps, ts)
		}
	}

	bt.timeStamps[volumeKey] = validTimestamps
}

// HasBudget checks if the volume has remaining budget for a resize action
// Returns true if the number of resize actions in the last 24 hours is less than maxActionsPerDay
func (bt *BudgetTracker) HasBudget(volumeKey string, maxActionsPerDay int) bool {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	bt.cleanExpiredEvents(volumeKey)

	currentCount := len(bt.timeStamps[volumeKey])
	return currentCount < maxActionsPerDay
}

// RecordResize records a resize action for a volume with the current timestamp
func (bt *BudgetTracker) RecordResize(volumeKey string) {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	bt.cleanExpiredEvents(volumeKey)

	if bt.timeStamps[volumeKey] == nil {
		bt.timeStamps[volumeKey] = make([]time.Time, 0)
	}

	bt.timeStamps[volumeKey] = append(bt.timeStamps[volumeKey], time.Now())
}

// RemainingBudget returns the number of resize actions still available within the 24-hour window
// Returns 0 if the budget is exhausted
func (bt *BudgetTracker) RemainingBudget(volumeKey string, maxActionsPerDay int) int {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	bt.cleanExpiredEvents(volumeKey)

	currentCount := len(bt.timeStamps[volumeKey])
	remaining := maxActionsPerDay - currentCount

	if remaining < 0 {
		return 0
	}

	return remaining
}
