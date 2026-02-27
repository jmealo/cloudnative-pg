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
	"k8s.io/utils/ptr"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("budget", func() {
	Describe("GetMaxActionsPerDay", func() {
		It("return default when config is nil", func() {
			Expect(GetMaxActionsPerDay(nil)).To(Equal(DefaultMaxActionsPerDay))
		})

		It("return default when EmergencyGrow is nil", func() {
			cfg := &apiv1.StorageConfiguration{}
			Expect(GetMaxActionsPerDay(cfg)).To(Equal(DefaultMaxActionsPerDay))
		})

		It("return configured value", func() {
			cfg := &apiv1.StorageConfiguration{
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					MaxActionsPerDay: ptr.To(10),
				},
			}
			Expect(GetMaxActionsPerDay(cfg)).To(Equal(10))
		})
	})

	Describe("GetReservedForEmergency", func() {
		It("return default when config is nil", func() {
			Expect(GetReservedForEmergency(nil)).To(Equal(DefaultReservedForEmergency))
		})

		It("return configured value", func() {
			cfg := &apiv1.StorageConfiguration{
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					ReservedActionsForEmergency: ptr.To(2),
				},
			}
			Expect(GetReservedForEmergency(cfg)).To(Equal(2))
		})
	})

	Describe("HasBudgetForEmergency", func() {
		It("return true for nil status", func() {
			Expect(HasBudgetForEmergency(nil, nil)).To(BeTrue())
		})

		It("return true when under budget", func() {
			status := &apiv1.VolumeSizingStatus{
				Budget: &apiv1.BudgetStatus{
					ActionsLast24h: 3,
				},
			}
			cfg := &apiv1.StorageConfiguration{
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					MaxActionsPerDay: ptr.To(4),
				},
			}
			Expect(HasBudgetForEmergency(cfg, status)).To(BeTrue())
		})

		It("return false when at budget", func() {
			status := &apiv1.VolumeSizingStatus{
				Budget: &apiv1.BudgetStatus{
					ActionsLast24h: 4,
				},
			}
			cfg := &apiv1.StorageConfiguration{
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					MaxActionsPerDay: ptr.To(4),
				},
			}
			Expect(HasBudgetForEmergency(cfg, status)).To(BeFalse())
		})
	})

	Describe("HasBudgetForScheduled", func() {
		It("return true for nil status", func() {
			Expect(HasBudgetForScheduled(nil, nil)).To(BeTrue())
		})

		It("return true when available for planned", func() {
			status := &apiv1.VolumeSizingStatus{
				Budget: &apiv1.BudgetStatus{
					AvailableForPlanned: 1,
				},
			}
			Expect(HasBudgetForScheduled(nil, status)).To(BeTrue())
		})

		It("return false when no available for planned", func() {
			status := &apiv1.VolumeSizingStatus{
				Budget: &apiv1.BudgetStatus{
					AvailableForPlanned: 0,
				},
			}
			Expect(HasBudgetForScheduled(nil, status)).To(BeFalse())
		})
	})

	Describe("CalculateBudget", func() {
		It("calculate initial budget correctly", func() {
			cfg := &apiv1.StorageConfiguration{
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					MaxActionsPerDay:            ptr.To(4),
					ReservedActionsForEmergency: ptr.To(1),
				},
			}
			budget := CalculateBudget(cfg, nil)
			Expect(budget.ActionsLast24h).To(Equal(0))
			Expect(budget.AvailableForEmergency).To(Equal(1))
			Expect(budget.AvailableForPlanned).To(Equal(3))
		})

		It("handle active actions within 24h", func() {
			cfg := &apiv1.StorageConfiguration{
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					MaxActionsPerDay:            ptr.To(4),
					ReservedActionsForEmergency: ptr.To(1),
				},
			}
			status := &apiv1.VolumeSizingStatus{
				LastAction: &apiv1.SizingAction{
					Timestamp: metav1.NewTime(time.Now().Add(-1 * time.Hour)),
				},
				Budget: &apiv1.BudgetStatus{
					ActionsLast24h: 2,
				},
			}
			budget := CalculateBudget(cfg, status)
			Expect(budget.ActionsLast24h).To(Equal(2))
			Expect(budget.AvailableForEmergency).To(Equal(1))
			Expect(budget.AvailableForPlanned).To(Equal(1))
		})

		It("reset budget if last action was > 24h ago", func() {
			cfg := &apiv1.StorageConfiguration{
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					MaxActionsPerDay: ptr.To(4),
				},
			}
			status := &apiv1.VolumeSizingStatus{
				LastAction: &apiv1.SizingAction{
					Timestamp: metav1.NewTime(time.Now().Add(-25 * time.Hour)),
				},
				Budget: &apiv1.BudgetStatus{
					ActionsLast24h: 4,
				},
			}
			budget := CalculateBudget(cfg, status)
			Expect(budget.ActionsLast24h).To(Equal(0))
		})
	})

	Describe("IncrementBudgetUsage", func() {
		It("correctly increment usage", func() {
			cfg := &apiv1.StorageConfiguration{
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					MaxActionsPerDay:            ptr.To(4),
					ReservedActionsForEmergency: ptr.To(1),
				},
			}
			status := &apiv1.VolumeSizingStatus{
				Budget: &apiv1.BudgetStatus{
					ActionsLast24h: 0,
				},
			}
			budget := IncrementBudgetUsage(cfg, status)
			Expect(budget.ActionsLast24h).To(Equal(1))
			Expect(budget.AvailableForEmergency).To(Equal(1))
			Expect(budget.AvailableForPlanned).To(Equal(2))
		})
	})
})
