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
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("sizing", func() {
	Describe("IsDynamicSizingEnabled", func() {
		It("return false for nil config", func() {
			Expect(IsDynamicSizingEnabled(nil)).To(BeFalse())
		})

		It("return false when only Size is set", func() {
			cfg := &apiv1.StorageConfiguration{
				Size: "10Gi",
			}
			Expect(IsDynamicSizingEnabled(cfg)).To(BeFalse())
		})

		It("return false when only Request is set", func() {
			cfg := &apiv1.StorageConfiguration{
				Request: "10Gi",
			}
			Expect(IsDynamicSizingEnabled(cfg)).To(BeFalse())
		})

		It("return false when only Limit is set", func() {
			cfg := &apiv1.StorageConfiguration{
				Limit: "100Gi",
			}
			Expect(IsDynamicSizingEnabled(cfg)).To(BeFalse())
		})

		It("return true when both Request and Limit are set", func() {
			cfg := &apiv1.StorageConfiguration{
				Request: "10Gi",
				Limit:   "100Gi",
			}
			Expect(IsDynamicSizingEnabled(cfg)).To(BeTrue())
		})
	})

	Describe("GetTargetBuffer", func() {
		It("return default when config is nil", func() {
			Expect(GetTargetBuffer(nil)).To(Equal(DefaultTargetBuffer))
		})

		It("return default when TargetBuffer is nil", func() {
			cfg := &apiv1.StorageConfiguration{}
			Expect(GetTargetBuffer(cfg)).To(Equal(DefaultTargetBuffer))
		})

		It("return configured value", func() {
			cfg := &apiv1.StorageConfiguration{
				TargetBuffer: ptr.To(30),
			}
			Expect(GetTargetBuffer(cfg)).To(Equal(30))
		})
	})

	Describe("CalculateTargetSize", func() {
		It("calculate correct target with 20% buffer", func() {
			usedBytes := uint64(8 * 1024 * 1024 * 1024) // 8 Gi
			target := CalculateTargetSize(usedBytes, 20)
			// 8 Gi / 0.8 = 10 Gi
			expectedBytes := int64(10 * 1024 * 1024 * 1024)
			Expect(target.Value()).To(Equal(expectedBytes))
		})

		It("calculate correct target with 25% buffer", func() {
			usedBytes := uint64(75 * 1024 * 1024 * 1024) // 75 Gi
			target := CalculateTargetSize(usedBytes, 25)
			// 75 Gi / 0.75 = 100 Gi
			expectedBytes := int64(100 * 1024 * 1024 * 1024)
			Expect(target.Value()).To(Equal(expectedBytes))
		})

		It("handle zero buffer by using default", func() {
			usedBytes := uint64(8 * 1024 * 1024 * 1024)
			target := CalculateTargetSize(usedBytes, 0)
			// Should use default 20% buffer
			expectedBytes := int64(10 * 1024 * 1024 * 1024)
			Expect(target.Value()).To(Equal(expectedBytes))
		})

		It("handle 100% buffer by using default", func() {
			usedBytes := uint64(8 * 1024 * 1024 * 1024)
			target := CalculateTargetSize(usedBytes, 100)
			// Should use default 20% buffer
			expectedBytes := int64(10 * 1024 * 1024 * 1024)
			Expect(target.Value()).To(Equal(expectedBytes))
		})
	})

	Describe("CalculateEmergencyGrowthSize", func() {
		It("grow by 25% of current size", func() {
			current := resource.MustParse("100Gi")
			limit := resource.MustParse("200Gi")
			result := CalculateEmergencyGrowthSize(current, limit)
			// 100 Gi + 25% = 125 Gi
			expected := resource.MustParse("125Gi")
			Expect(result.Cmp(expected)).To(Equal(0))
		})

		It("respect the limit", func() {
			current := resource.MustParse("90Gi")
			limit := resource.MustParse("100Gi")
			result := CalculateEmergencyGrowthSize(current, limit)
			// 90 Gi + 25% = 112.5 Gi, but limit is 100 Gi
			Expect(result.Cmp(limit)).To(Equal(0))
		})

		It("ensure minimum growth of 1Gi", func() {
			current := resource.MustParse("2Gi")
			limit := resource.MustParse("100Gi")
			result := CalculateEmergencyGrowthSize(current, limit)
			// 2 Gi + 25% = 2.5 Gi, but min growth is 1 Gi, so 3 Gi
			expected := resource.MustParse("3Gi")
			Expect(result.Cmp(expected)).To(Equal(0))
		})
	})

	Describe("ClampSize", func() {
		It("return target when within bounds", func() {
			target := resource.MustParse("50Gi")
			request := resource.MustParse("10Gi")
			limit := resource.MustParse("100Gi")
			result := ClampSize(target, request, limit)
			Expect(result.Cmp(target)).To(Equal(0))
		})

		It("return request when target is below", func() {
			target := resource.MustParse("5Gi")
			request := resource.MustParse("10Gi")
			limit := resource.MustParse("100Gi")
			result := ClampSize(target, request, limit)
			Expect(result.Cmp(request)).To(Equal(0))
		})

		It("return limit when target is above", func() {
			target := resource.MustParse("150Gi")
			request := resource.MustParse("10Gi")
			limit := resource.MustParse("100Gi")
			result := ClampSize(target, request, limit)
			Expect(result.Cmp(limit)).To(Equal(0))
		})
	})

	Describe("IsEmergencyCondition", func() {
		It("return false when under threshold", func() {
			cfg := &apiv1.StorageConfiguration{
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					CriticalThreshold: 95,
				},
			}
			total := uint64(100 * 1024 * 1024 * 1024)    // 100 Gi
			used := uint64(90 * 1024 * 1024 * 1024)      // 90 Gi (90%)
			available := uint64(10 * 1024 * 1024 * 1024) // 10 Gi
			Expect(IsEmergencyCondition(cfg, total, used, available)).To(BeFalse())
		})

		It("return true when at threshold", func() {
			cfg := &apiv1.StorageConfiguration{
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					CriticalThreshold: 95,
				},
			}
			total := uint64(100 * 1024 * 1024 * 1024)   // 100 Gi
			used := uint64(95 * 1024 * 1024 * 1024)     // 95 Gi (95%)
			available := uint64(5 * 1024 * 1024 * 1024) // 5 Gi
			Expect(IsEmergencyCondition(cfg, total, used, available)).To(BeTrue())
		})

		It("return true when below minimum free", func() {
			cfg := &apiv1.StorageConfiguration{
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					CriticalThreshold:   95,
					CriticalMinimumFree: "2Gi",
				},
			}
			total := uint64(100 * 1024 * 1024 * 1024)   // 100 Gi
			used := uint64(80 * 1024 * 1024 * 1024)     // 80 Gi (80%)
			available := uint64(1 * 1024 * 1024 * 1024) // 1 Gi - below 2 Gi minimum
			Expect(IsEmergencyCondition(cfg, total, used, available)).To(BeTrue())
		})

		It("return false when emergency grow is disabled", func() {
			cfg := &apiv1.StorageConfiguration{
				EmergencyGrow: &apiv1.EmergencyGrowConfig{
					Enabled:           ptr.To(false),
					CriticalThreshold: 95,
				},
			}
			total := uint64(100 * 1024 * 1024 * 1024)
			used := uint64(99 * 1024 * 1024 * 1024)
			available := uint64(1 * 1024 * 1024 * 1024)
			Expect(IsEmergencyCondition(cfg, total, used, available)).To(BeFalse())
		})
	})

	Describe("NeedsGrowth", func() {
		It("return true when free space is below target buffer", func() {
			cfg := &apiv1.StorageConfiguration{
				TargetBuffer: ptr.To(20),
			}
			total := uint64(100 * 1024 * 1024 * 1024) // 100 Gi
			used := uint64(85 * 1024 * 1024 * 1024)   // 85 Gi (15% free, below 20%)
			Expect(NeedsGrowth(cfg, total, used)).To(BeTrue())
		})

		It("return false when free space is at target buffer", func() {
			cfg := &apiv1.StorageConfiguration{
				TargetBuffer: ptr.To(20),
			}
			total := uint64(100 * 1024 * 1024 * 1024) // 100 Gi
			used := uint64(80 * 1024 * 1024 * 1024)   // 80 Gi (20% free)
			Expect(NeedsGrowth(cfg, total, used)).To(BeFalse())
		})

		It("return false when free space is above target buffer", func() {
			cfg := &apiv1.StorageConfiguration{
				TargetBuffer: ptr.To(20),
			}
			total := uint64(100 * 1024 * 1024 * 1024) // 100 Gi
			used := uint64(70 * 1024 * 1024 * 1024)   // 70 Gi (30% free)
			Expect(NeedsGrowth(cfg, total, used)).To(BeFalse())
		})

		It("return false for zero total", func() {
			cfg := &apiv1.StorageConfiguration{}
			Expect(NeedsGrowth(cfg, 0, 0)).To(BeFalse())
		})
	})
})
