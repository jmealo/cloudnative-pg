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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsCanBeRegistered(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics()

	if err := metrics.Register(registry); err != nil {
		t.Fatalf("failed to register metrics: %v", err)
	}

	// Verify we can set values
	metrics.TotalBytes.WithLabelValues("data", "").Set(100)
	metrics.UsedBytes.WithLabelValues("data", "").Set(80)
	metrics.PercentUsed.WithLabelValues("data", "").Set(80.0)
}

func TestMetricsLabels(t *testing.T) {
	metrics := NewMetrics()

	// Should support data, wal, and tablespace volume types
	metrics.TotalBytes.WithLabelValues("data", "").Set(100)
	metrics.TotalBytes.WithLabelValues("wal", "").Set(50)
	metrics.TotalBytes.WithLabelValues("tablespace", "hot_data").Set(200)
}

func TestSetVolumeStats(t *testing.T) {
	metrics := NewMetrics()

	stats := &VolumeStats{
		Path:           "/var/lib/postgresql/data",
		TotalBytes:     100 * 1024 * 1024 * 1024, // 100Gi
		UsedBytes:      80 * 1024 * 1024 * 1024,  // 80Gi
		AvailableBytes: 20 * 1024 * 1024 * 1024,  // 20Gi
		PercentUsed:    80,
		InodesTotal:    1000000,
		InodesUsed:     500000,
		InodesFree:     500000,
	}

	// Should not panic
	metrics.SetVolumeStats("data", "", stats)
}

func TestSetVolumeStatsNil(t *testing.T) {
	metrics := NewMetrics()

	// Should not panic with nil stats
	metrics.SetVolumeStats("data", "", nil)
}
