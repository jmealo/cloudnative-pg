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

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("maintenance", func() {
	Describe("IsMaintenanceWindowOpen", func() {
		It("return true for nil config", func() {
			Expect(IsMaintenanceWindowOpen(nil)).To(BeTrue())
		})

		It("return true when maintenance window is nil", func() {
			cfg := &apiv1.StorageConfiguration{}
			Expect(IsMaintenanceWindowOpen(cfg)).To(BeTrue())
		})
	})

	Describe("findMostRecentWindowStart", func() {
		It("find yesterday's window if today's hasn't started", func() {
			// Daily at 3 AM (using 6 fields: sec min hour dom month dow)
			schedule := "0 0 3 * * *"
			cronSchedule, _ := cronParser.Parse(schedule)

			// Today is Feb 8, 2 AM. Most recent should be Feb 7, 3 AM.
			now := time.Date(2026, 2, 8, 2, 0, 0, 0, time.UTC)
			result := findMostRecentWindowStart(cronSchedule, now, 24*time.Hour)

			expected := time.Date(2026, 2, 7, 3, 0, 0, 0, time.UTC)
			Expect(result.Unix()).To(Equal(expected.Unix()))
		})

		It("find today's window if it has started", func() {
			// Daily at 3 AM
			schedule := "0 0 3 * * *"
			cronSchedule, _ := cronParser.Parse(schedule)

			// Today is Feb 8, 4 AM. Most recent should be Feb 8, 3 AM.
			now := time.Date(2026, 2, 8, 4, 0, 0, 0, time.UTC)
			result := findMostRecentWindowStart(cronSchedule, now, 24*time.Hour)

			expected := time.Date(2026, 2, 8, 3, 0, 0, 0, time.UTC)
			Expect(result.Unix()).To(Equal(expected.Unix()))
		})
	})

	Describe("NextMaintenanceWindow", func() {
		It("return nil for nil config", func() {
			Expect(NextMaintenanceWindow(nil)).To(BeNil())
		})

		It("calculate next window correctly", func() {
			cfg := &apiv1.StorageConfiguration{
				MaintenanceWindow: &apiv1.MaintenanceWindowConfig{
					Schedule: "0 0 3 * * *",
				},
			}
			result := NextMaintenanceWindow(cfg)
			Expect(result).ToNot(BeNil())
			Expect(result.After(time.Now())).To(BeTrue())
		})
	})
})
