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

package metricserver

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/cloudnative-pg/cloudnative-pg/pkg/management/postgres"
	postgresstatus "github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("diskCollector", func() {
	It("should not return duplicate metrics when multiple volumes are present", func() {
		// Mock the status to have both data and wal volumes
		status := &postgresstatus.PostgresqlStatus{
			DiskStatus: &postgresstatus.DiskStatus{
				TotalBytes:     1000,
				UsedBytes:      500,
				AvailableBytes: 500,
				PercentUsed:    50,
			},
			WALDiskStatus: &postgresstatus.DiskStatus{
				TotalBytes:     2000,
				UsedBytes:      200,
				AvailableBytes: 1800,
				PercentUsed:    10,
			},
		}

		instance := postgres.NewInstance()
		collector := newDiskCollector(instance)

		// Override getStatus and getPodName for testing
		collector.getStatus = func() (*postgresstatus.PostgresqlStatus, error) {
			return status, nil
		}
		collector.getPodName = func() string {
			return "test-pod"
		}

		reg := prometheus.NewRegistry()
		err := reg.Register(collector)
		Expect(err).ToNot(HaveOccurred())

		metricFamilies, err := reg.Gather()
		Expect(err).ToNot(HaveOccurred())

		for mfName, expectedCount := range map[string]int{
			"cnpg_disk_total_bytes":     2,
			"cnpg_disk_used_bytes":      2,
			"cnpg_disk_available_bytes": 2,
			"cnpg_disk_percent_used":    2,
		} {
			mf := getMetricFamily(metricFamilies, mfName)
			Expect(mf).ToNot(BeNil(), "Metric family %s not found", mfName)
			Expect(mf.GetMetric()).To(HaveLen(expectedCount),
				"Metric family %s should have exactly %d metrics", mfName, expectedCount)

			// Check for duplicates by ensuring label combinations are unique
			labelsSeen := make(map[string]struct{})
			for _, m := range mf.GetMetric() {
				labelStr := formatLabels(m.GetLabel())
				_, seen := labelsSeen[labelStr]
				Expect(seen).To(BeFalse(), "Duplicate metric found for %s with labels %s", mfName, labelStr)
				labelsSeen[labelStr] = struct{}{}
			}
		}
	})
})

func getMetricFamily(mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

func formatLabels(labels []*dto.LabelPair) string {
	res := ""
	for _, l := range labels {
		res += l.GetName() + "=" + l.GetValue() + ","
	}
	return res
}
