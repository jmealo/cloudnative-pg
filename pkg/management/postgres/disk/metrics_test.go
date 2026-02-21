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

package disk

import (
	"github.com/prometheus/client_golang/prometheus"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Disk Metrics", func() {
	Describe("NewMetrics", func() {
		It("can be registered with prometheus", func() {
			registry := prometheus.NewRegistry()
			metrics := NewMetrics()

			err := metrics.Register(registry)
			Expect(err).ToNot(HaveOccurred())

			// Verify we can set values
			// Note: percent_used can be calculated via PromQL as:
			// cnpg_disk_used_bytes / cnpg_disk_total_bytes * 100
			metrics.TotalBytes.WithLabelValues("data", "").Set(100)
			metrics.UsedBytes.WithLabelValues("data", "").Set(80)
			metrics.AvailableBytes.WithLabelValues("data", "").Set(20)
		})

		It("supports multiple volume types", func() {
			metrics := NewMetrics()

			// Should support data, wal, and tablespace volume types
			metrics.TotalBytes.WithLabelValues("data", "").Set(100)
			metrics.TotalBytes.WithLabelValues("wal", "").Set(50)
			metrics.TotalBytes.WithLabelValues("tablespace", "hot_data").Set(200)
		})
	})

	Describe("SetVolumeStats", func() {
		It("updates metrics from VolumeStats", func() {
			metrics := NewMetrics()

			stats := &VolumeStats{
				Path:           "/var/lib/postgresql/data",
				TotalBytes:     100 * 1024 * 1024 * 1024,
				UsedBytes:      80 * 1024 * 1024 * 1024,
				AvailableBytes: 20 * 1024 * 1024 * 1024,
				PercentUsed:    80,
				InodesTotal:    1000000,
				InodesUsed:     500000,
				InodesFree:     500000,
			}

			// Should not panic
			metrics.SetVolumeStats("data", "", stats)
		})

		It("handles nil stats gracefully", func() {
			metrics := NewMetrics()

			// Should not panic with nil stats
			metrics.SetVolumeStats("data", "", nil)
		})
	})
})
