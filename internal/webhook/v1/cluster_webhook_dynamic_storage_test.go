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

package v1

import (
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("dynamic storage validation", func() {
	var v *ClusterCustomValidator
	BeforeEach(func() {
		v = &ClusterCustomValidator{}
	})

	Describe("validateStorageConfigurationSize", func() {
		Context("static sizing mode (size field)", func() {
			It("accept valid static size", func() {
				cfg := apiv1.StorageConfiguration{
					Size: "10Gi",
				}
				result := validateStorageConfigurationSize(*field.NewPath("spec", "storage"), cfg)
				Expect(result).To(BeEmpty())
			})

			It("reject invalid static size", func() {
				cfg := apiv1.StorageConfiguration{
					Size: "invalid",
				}
				result := validateStorageConfigurationSize(*field.NewPath("spec", "storage"), cfg)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("Size value isn't valid"))
			})
		})

		Context("dynamic sizing mode (request/limit fields)", func() {
			It("accept valid dynamic configuration", func() {
				cfg := apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "100Gi",
				}
				result := validateStorageConfigurationSize(*field.NewPath("spec", "storage"), cfg)
				Expect(result).To(BeEmpty())
			})

			It("reject request without limit", func() {
				cfg := apiv1.StorageConfiguration{
					Request: "10Gi",
				}
				result := validateStorageConfigurationSize(*field.NewPath("spec", "storage"), cfg)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("limit is required"))
			})

			It("reject limit without request", func() {
				cfg := apiv1.StorageConfiguration{
					Limit: "100Gi",
				}
				result := validateStorageConfigurationSize(*field.NewPath("spec", "storage"), cfg)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("request is required"))
			})

			It("reject request greater than limit", func() {
				cfg := apiv1.StorageConfiguration{
					Request: "100Gi",
					Limit:   "10Gi",
				}
				result := validateStorageConfigurationSize(*field.NewPath("spec", "storage"), cfg)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("cannot exceed limit"))
			})

			It("reject invalid request value", func() {
				cfg := apiv1.StorageConfiguration{
					Request: "invalid",
					Limit:   "100Gi",
				}
				result := validateStorageConfigurationSize(*field.NewPath("spec", "storage"), cfg)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("request value isn't valid"))
			})

			It("reject invalid limit value", func() {
				cfg := apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "invalid",
				}
				result := validateStorageConfigurationSize(*field.NewPath("spec", "storage"), cfg)
				// May have multiple errors (limit invalid, and possibly request > limit comparison)
				Expect(result).NotTo(BeEmpty())
				var foundLimitError bool
				for _, e := range result {
					if strings.Contains(e.Error(), "limit value isn't valid") {
						foundLimitError = true
						break
					}
				}
				Expect(foundLimitError).To(BeTrue())
			})
		})

		Context("mutual exclusivity", func() {
			It("reject size with request", func() {
				cfg := apiv1.StorageConfiguration{
					Size:    "10Gi",
					Request: "10Gi",
				}
				result := validateStorageConfigurationSize(*field.NewPath("spec", "storage"), cfg)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("mutually exclusive"))
			})

			It("reject size with limit", func() {
				cfg := apiv1.StorageConfiguration{
					Size:  "10Gi",
					Limit: "100Gi",
				}
				result := validateStorageConfigurationSize(*field.NewPath("spec", "storage"), cfg)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("mutually exclusive"))
			})

			It("reject size with both request and limit", func() {
				cfg := apiv1.StorageConfiguration{
					Size:    "10Gi",
					Request: "10Gi",
					Limit:   "100Gi",
				}
				result := validateStorageConfigurationSize(*field.NewPath("spec", "storage"), cfg)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("mutually exclusive"))
			})
		})
	})

	Describe("validateMaintenanceWindow", func() {
		It("accept valid cron schedule", func() {
			mw := &apiv1.MaintenanceWindowConfig{
				Schedule: "0 0 3 * * *",
				Duration: "2h",
				Timezone: "UTC",
			}
			result := validateMaintenanceWindow(*field.NewPath("maintenanceWindow"), mw)
			Expect(result).To(BeEmpty())
		})

		It("reject invalid cron schedule (wrong number of fields)", func() {
			mw := &apiv1.MaintenanceWindowConfig{
				Schedule: "0 3 * *", // Missing field
			}
			result := validateMaintenanceWindow(*field.NewPath("maintenanceWindow"), mw)
			Expect(result).To(HaveLen(1))
			Expect(result[0].Error()).To(ContainSubstring("invalid cron schedule"))
		})

		It("reject invalid duration", func() {
			mw := &apiv1.MaintenanceWindowConfig{
				Schedule: "0 0 3 * * *",
				Duration: "invalid",
			}
			result := validateMaintenanceWindow(*field.NewPath("maintenanceWindow"), mw)
			Expect(result).To(HaveLen(1))
			Expect(result[0].Error()).To(ContainSubstring("invalid duration"))
		})

		It("accept various valid durations", func() {
			durations := []string{"1h", "30m", "1h30m", "24h"}
			for _, d := range durations {
				mw := &apiv1.MaintenanceWindowConfig{
					Schedule: "0 0 3 * * *",
					Duration: d,
				}
				result := validateMaintenanceWindow(*field.NewPath("maintenanceWindow"), mw)
				Expect(result).To(BeEmpty(), "Duration %s should be valid", d)
			}
		})
	})

	Describe("validateEmergencyGrowConfig", func() {
		It("accept valid configuration", func() {
			eg := &apiv1.EmergencyGrowConfig{
				Enabled:             ptr.To(true),
				CriticalThreshold:   95,
				CriticalMinimumFree: "1Gi",
				MaxActionsPerDay:    ptr.To(4),
			}
			result := validateEmergencyGrowConfig(*field.NewPath("emergencyGrow"), eg)
			Expect(result).To(BeEmpty())
		})

		It("reject invalid criticalMinimumFree", func() {
			eg := &apiv1.EmergencyGrowConfig{
				CriticalMinimumFree: "invalid",
			}
			result := validateEmergencyGrowConfig(*field.NewPath("emergencyGrow"), eg)
			Expect(result).To(HaveLen(1))
			Expect(result[0].Error()).To(ContainSubstring("invalid quantity"))
		})

		It("reject negative maxActionsPerDay", func() {
			eg := &apiv1.EmergencyGrowConfig{
				MaxActionsPerDay: ptr.To(-1),
			}
			result := validateEmergencyGrowConfig(*field.NewPath("emergencyGrow"), eg)
			Expect(result).To(HaveLen(1))
			Expect(result[0].Error()).To(ContainSubstring("cannot be negative"))
		})

		It("reject reserved actions exceeding max actions", func() {
			eg := &apiv1.EmergencyGrowConfig{
				MaxActionsPerDay:            ptr.To(2),
				ReservedActionsForEmergency: ptr.To(3),
			}
			result := validateEmergencyGrowConfig(*field.NewPath("emergencyGrow"), eg)
			Expect(result).To(HaveLen(1))
			Expect(result[0].Error()).To(ContainSubstring("cannot exceed maxActionsPerDay"))
		})
	})

	Describe("validateStaticToDynamicMigration", func() {
		Context("with status data (actual PVC sizes)", func() {
			It("allow migration when request equals minimum actual PVC size", func() {
				oldStorage := apiv1.StorageConfiguration{
					Size: "10Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Request: "50Gi",
					Limit:   "100Gi",
				}
				volumeStatus := &apiv1.VolumeSizingStatus{
					ActualSizes: map[string]string{
						"instance-1": "50Gi",
						"instance-2": "60Gi",
					},
				}
				result := validateStaticToDynamicMigration(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
					volumeStatus,
				)
				Expect(result).To(BeEmpty())
			})

			It("allow migration when request is less than minimum actual PVC size", func() {
				oldStorage := apiv1.StorageConfiguration{
					Size: "10Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Request: "20Gi",
					Limit:   "100Gi",
				}
				volumeStatus := &apiv1.VolumeSizingStatus{
					ActualSizes: map[string]string{
						"instance-1": "50Gi",
						"instance-2": "60Gi",
					},
				}
				result := validateStaticToDynamicMigration(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
					volumeStatus,
				)
				Expect(result).To(BeEmpty())
			})

			It("reject migration when request exceeds minimum actual PVC size", func() {
				oldStorage := apiv1.StorageConfiguration{
					Size: "10Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Request: "55Gi",
					Limit:   "100Gi",
				}
				volumeStatus := &apiv1.VolumeSizingStatus{
					ActualSizes: map[string]string{
						"instance-1": "50Gi",
						"instance-2": "60Gi",
					},
				}
				result := validateStaticToDynamicMigration(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
					volumeStatus,
				)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("request cannot exceed minimum actual PVC size (50Gi)"))
			})

			It("reject migration when limit is less than minimum actual PVC size", func() {
				oldStorage := apiv1.StorageConfiguration{
					Size: "10Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "40Gi",
				}
				volumeStatus := &apiv1.VolumeSizingStatus{
					ActualSizes: map[string]string{
						"instance-1": "50Gi",
						"instance-2": "60Gi",
					},
				}
				result := validateStaticToDynamicMigration(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
					volumeStatus,
				)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("limit cannot be less than minimum actual PVC size (50Gi)"))
			})
		})

		Context("without status data (fallback to spec)", func() {
			It("allow migration when request equals spec size", func() {
				oldStorage := apiv1.StorageConfiguration{
					Size: "10Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "100Gi",
				}
				result := validateStaticToDynamicMigration(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
					nil,
				)
				Expect(result).To(BeEmpty())
			})

			It("allow migration when request is less than spec size", func() {
				oldStorage := apiv1.StorageConfiguration{
					Size: "50Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "100Gi",
				}
				result := validateStaticToDynamicMigration(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
					nil,
				)
				Expect(result).To(BeEmpty())
			})

			It("reject migration when request exceeds spec size", func() {
				oldStorage := apiv1.StorageConfiguration{
					Size: "10Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Request: "20Gi",
					Limit:   "100Gi",
				}
				result := validateStaticToDynamicMigration(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
					nil,
				)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("request cannot exceed minimum actual PVC size (10Gi)"))
			})

			It("reject migration when limit is less than spec size", func() {
				oldStorage := apiv1.StorageConfiguration{
					Size: "50Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "40Gi",
				}
				result := validateStaticToDynamicMigration(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
					nil,
				)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("limit cannot be less than minimum actual PVC size (50Gi)"))
			})
		})
	})

	Describe("validateStorageConfigurationChange", func() {
		Context("mode switching", func() {
			It("delegate static to dynamic migration to validateStaticToDynamicMigration", func() {
				oldStorage := apiv1.StorageConfiguration{
					Size: "10Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "100Gi",
				}
				result := validateStorageConfigurationChange(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
				)
				Expect(result).To(BeEmpty())
			})

			It("reject switching from dynamic to static mode", func() {
				oldStorage := apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "100Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Size: "50Gi",
				}
				result := validateStorageConfigurationChange(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
				)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("cannot switch from dynamic sizing"))
			})
		})

		Context("static mode changes", func() {
			It("allow increasing size", func() {
				oldStorage := apiv1.StorageConfiguration{
					Size: "10Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Size: "20Gi",
				}
				result := validateStorageConfigurationChange(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
				)
				Expect(result).To(BeEmpty())
			})

			It("reject decreasing size", func() {
				oldStorage := apiv1.StorageConfiguration{
					Size: "20Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Size: "10Gi",
				}
				result := validateStorageConfigurationChange(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
				)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("can't shrink"))
			})
		})

		Context("dynamic mode changes", func() {
			It("allow changing request and limit within bounds", func() {
				oldStorage := apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "100Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Request: "20Gi",
					Limit:   "200Gi",
				}
				result := validateStorageConfigurationChange(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
				)
				Expect(result).To(BeEmpty())
			})

			It("allow decreasing limit", func() {
				oldStorage := apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "100Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "50Gi",
				}
				result := validateStorageConfigurationChange(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
				)
				Expect(result).To(BeEmpty())
			})

			It("reject new request greater than new limit", func() {
				oldStorage := apiv1.StorageConfiguration{
					Request: "10Gi",
					Limit:   "100Gi",
				}
				newStorage := apiv1.StorageConfiguration{
					Request: "60Gi",
					Limit:   "50Gi",
				}
				result := validateStorageConfigurationChange(
					field.NewPath("spec", "storage"),
					oldStorage,
					newStorage,
				)
				Expect(result).To(HaveLen(1))
				Expect(result[0].Error()).To(ContainSubstring("cannot exceed limit"))
			})
		})
	})

	Describe("cluster validation with dynamic storage", func() {
		It("accept cluster with valid dynamic storage", func() {
			cluster := &apiv1.Cluster{
				Spec: apiv1.ClusterSpec{
					Instances: 3,
					StorageConfiguration: apiv1.StorageConfiguration{
						Request: "10Gi",
						Limit:   "100Gi",
					},
				},
			}
			result := v.validateStorageSize(cluster)
			Expect(result).To(BeEmpty())
		})

		It("validate dynamic storage with maintenance window", func() {
			cluster := &apiv1.Cluster{
				Spec: apiv1.ClusterSpec{
					Instances: 3,
					StorageConfiguration: apiv1.StorageConfiguration{
						Request: "10Gi",
						Limit:   "100Gi",
						MaintenanceWindow: &apiv1.MaintenanceWindowConfig{
							Schedule: "0 0 3 * * 0",
							Duration: "4h",
						},
					},
				},
			}
			result := v.validateStorageSize(cluster)
			Expect(result).To(BeEmpty())
		})
	})
})
