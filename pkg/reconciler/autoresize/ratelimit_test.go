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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("BudgetTracker", func() {
	var (
		tracker          *BudgetTracker
		volumeKey        string
		maxActionsPerDay int
	)

	BeforeEach(func() {
		tracker = NewBudgetTracker()
		volumeKey = "default/pvc-test"
		maxActionsPerDay = 3
	})

	Context("Fresh tracker", func() {
		It("should have budget available", func() {
			result := tracker.HasBudget(volumeKey, maxActionsPerDay)

			Expect(result).To(BeTrue())
		})

		It("should return full remaining budget", func() {
			remaining := tracker.RemainingBudget(volumeKey, maxActionsPerDay)

			Expect(remaining).To(Equal(maxActionsPerDay))
		})
	})

	Context("Recording resize actions", func() {
		It("should decrease remaining budget after recording one resize", func() {
			// Initial budget should be 3
			remaining := tracker.RemainingBudget(volumeKey, maxActionsPerDay)
			Expect(remaining).To(Equal(3))

			// Record one resize
			tracker.RecordResize(volumeKey)

			// Remaining budget should be 2
			remaining = tracker.RemainingBudget(volumeKey, maxActionsPerDay)
			Expect(remaining).To(Equal(2))
		})

		It("should track multiple resize actions", func() {
			tracker.RecordResize(volumeKey)
			tracker.RecordResize(volumeKey)

			remaining := tracker.RemainingBudget(volumeKey, maxActionsPerDay)

			Expect(remaining).To(Equal(1))
		})
	})

	Context("Budget exhaustion", func() {
		It("should exhaust budget after maxActionsPerDay resize actions", func() {
			// Record 3 resize actions (maxActionsPerDay = 3)
			tracker.RecordResize(volumeKey)
			tracker.RecordResize(volumeKey)
			tracker.RecordResize(volumeKey)

			// Should have no budget left
			hasBudget := tracker.HasBudget(volumeKey, maxActionsPerDay)
			Expect(hasBudget).To(BeFalse())

			remaining := tracker.RemainingBudget(volumeKey, maxActionsPerDay)
			Expect(remaining).To(Equal(0))
		})

		It("should continue to return false when budget is exhausted", func() {
			// Exhaust the budget
			tracker.RecordResize(volumeKey)
			tracker.RecordResize(volumeKey)
			tracker.RecordResize(volumeKey)

			// Try to record one more (should still track internally)
			tracker.RecordResize(volumeKey)

			// Still no budget
			hasBudget := tracker.HasBudget(volumeKey, maxActionsPerDay)
			Expect(hasBudget).To(BeFalse())
		})
	})

	Context("Budget rollover", func() {
		It("should recover budget after 24 hours", func() {
			// Record 3 resize actions
			tracker.RecordResize(volumeKey)
			tracker.RecordResize(volumeKey)
			tracker.RecordResize(volumeKey)

			// Budget should be exhausted
			Expect(tracker.HasBudget(volumeKey, maxActionsPerDay)).To(BeFalse())

			// Manually manipulate timestamps to simulate 24+ hours passing
			tracker.mu.Lock()
			if len(tracker.timeStamps[volumeKey]) > 0 {
				// Set all timestamps to 25 hours ago
				pastTime := time.Now().Add(-25 * time.Hour)
				for i := range tracker.timeStamps[volumeKey] {
					tracker.timeStamps[volumeKey][i] = pastTime
				}
			}
			tracker.mu.Unlock()

			// Budget should be recovered (all old timestamps cleaned up)
			remaining := tracker.RemainingBudget(volumeKey, maxActionsPerDay)
			Expect(remaining).To(Equal(maxActionsPerDay))

			hasBudget := tracker.HasBudget(volumeKey, maxActionsPerDay)
			Expect(hasBudget).To(BeTrue())
		})

		It("should partially recover budget as old timestamps expire", func() {
			// Record 3 resize actions
			tracker.RecordResize(volumeKey)
			tracker.RecordResize(volumeKey)
			tracker.RecordResize(volumeKey)

			// Remaining should be 0
			remaining := tracker.RemainingBudget(volumeKey, maxActionsPerDay)
			Expect(remaining).To(Equal(0))

			// Manually set first two timestamps to 25 hours ago (they'll be cleaned up)
			// Keep the last one as recent
			tracker.mu.Lock()
			if len(tracker.timeStamps[volumeKey]) >= 3 {
				pastTime := time.Now().Add(-25 * time.Hour)
				tracker.timeStamps[volumeKey][0] = pastTime
				tracker.timeStamps[volumeKey][1] = pastTime
				// tracker.timeStamps[volumeKey][2] stays as recent (now)
			}
			tracker.mu.Unlock()

			// After cleanup, should have 2 slots available
			remaining = tracker.RemainingBudget(volumeKey, maxActionsPerDay)
			Expect(remaining).To(Equal(2))
		})
	})

	Context("Different volumes", func() {
		It("should track budget separately for each volume", func() {
			volumeKey1 := "default/pvc-1"
			volumeKey2 := "default/pvc-2"

			// Record resize for first volume twice
			tracker.RecordResize(volumeKey1)
			tracker.RecordResize(volumeKey1)

			// Record resize for second volume once
			tracker.RecordResize(volumeKey2)

			// First volume should have 1 slot remaining
			remaining1 := tracker.RemainingBudget(volumeKey1, maxActionsPerDay)
			Expect(remaining1).To(Equal(1))

			// Second volume should have 2 slots remaining
			remaining2 := tracker.RemainingBudget(volumeKey2, maxActionsPerDay)
			Expect(remaining2).To(Equal(2))
		})

		It("should exhaust one volume's budget without affecting another", func() {
			volumeKey1 := "default/pvc-1"
			volumeKey2 := "default/pvc-2"

			// Exhaust budget for first volume
			tracker.RecordResize(volumeKey1)
			tracker.RecordResize(volumeKey1)
			tracker.RecordResize(volumeKey1)

			// Second volume should still have budget
			hasBudget1 := tracker.HasBudget(volumeKey1, maxActionsPerDay)
			hasBudget2 := tracker.HasBudget(volumeKey2, maxActionsPerDay)

			Expect(hasBudget1).To(BeFalse())
			Expect(hasBudget2).To(BeTrue())
		})
	})

	Context("Edge cases", func() {
		It("should handle zero maxActionsPerDay", func() {
			maxActions := 0

			hasBudget := tracker.HasBudget(volumeKey, maxActions)
			Expect(hasBudget).To(BeFalse())

			remaining := tracker.RemainingBudget(volumeKey, maxActions)
			Expect(remaining).To(Equal(0))
		})

		It("should handle high maxActionsPerDay values", func() {
			highLimit := 100

			remaining := tracker.RemainingBudget(volumeKey, highLimit)
			Expect(remaining).To(Equal(100))

			hasBudget := tracker.HasBudget(volumeKey, highLimit)
			Expect(hasBudget).To(BeTrue())
		})

		It("should be thread-safe with concurrent operations", func() {
			done := make(chan struct{})
			volumeKey := "concurrent/pvc"

			// Launch multiple goroutines recording resizes
			for i := 0; i < 5; i++ {
				go func() {
					tracker.RecordResize(volumeKey)
					done <- struct{}{}
				}()
			}

			// Wait for all goroutines
			for i := 0; i < 5; i++ {
				<-done
			}

			// Should have recorded 5 actions
			remaining := tracker.RemainingBudget(volumeKey, 10)
			Expect(remaining).To(Equal(5))
		})
	})
})
