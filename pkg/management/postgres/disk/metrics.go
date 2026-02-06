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
)

const (
	namespace = "cnpg"
	subsystem = "disk"
)

// Metrics contains all disk-related Prometheus metrics
type Metrics struct {
	// Bytes metrics
	TotalBytes     *prometheus.GaugeVec
	UsedBytes      *prometheus.GaugeVec
	AvailableBytes *prometheus.GaugeVec
	PercentUsed    *prometheus.GaugeVec

	// Inode metrics
	InodesTotal *prometheus.GaugeVec
	InodesUsed  *prometheus.GaugeVec
	InodesFree  *prometheus.GaugeVec

	// Auto-resize status
	AtMaxSize     *prometheus.GaugeVec
	ResizeBlocked *prometheus.GaugeVec
	ResizesTotal  *prometheus.CounterVec
}

// NewMetrics creates disk metrics
func NewMetrics() *Metrics {
	labels := []string{"volume_type", "tablespace"}

	return &Metrics{
		TotalBytes: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "total_bytes",
				Help:      "Total capacity of the volume in bytes",
			},
			labels,
		),
		UsedBytes: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "used_bytes",
				Help:      "Used space on the volume in bytes",
			},
			labels,
		),
		AvailableBytes: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "available_bytes",
				Help:      "Available space on the volume in bytes",
			},
			labels,
		),
		PercentUsed: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "percent_used",
				Help:      "Percentage of volume space used",
			},
			labels,
		),
		InodesTotal: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "inodes_total",
				Help:      "Total inodes on the volume",
			},
			labels,
		),
		InodesUsed: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "inodes_used",
				Help:      "Used inodes on the volume",
			},
			labels,
		),
		InodesFree: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "inodes_free",
				Help:      "Free inodes on the volume",
			},
			labels,
		),
		AtMaxSize: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "at_max_size",
				Help:      "1 if volume has reached configured maxSize limit",
			},
			labels,
		),
		ResizeBlocked: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "resize_blocked",
				Help:      "1 if auto-resize is blocked (WAL health, cooldown, etc.)",
			},
			append(labels, "reason"),
		),
		ResizesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "resizes_total",
				Help:      "Total number of auto-resize operations",
			},
			append(labels, "result"),
		),
	}
}

// Register registers all metrics with the provided registry
func (m *Metrics) Register(registry prometheus.Registerer) error {
	collectors := []prometheus.Collector{
		m.TotalBytes,
		m.UsedBytes,
		m.AvailableBytes,
		m.PercentUsed,
		m.InodesTotal,
		m.InodesUsed,
		m.InodesFree,
		m.AtMaxSize,
		m.ResizeBlocked,
		m.ResizesTotal,
	}

	for _, c := range collectors {
		if err := registry.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// SetVolumeStats updates metrics from VolumeStats
func (m *Metrics) SetVolumeStats(volumeType, tablespace string, stats *VolumeStats) {
	if stats == nil {
		return
	}
	m.TotalBytes.WithLabelValues(volumeType, tablespace).Set(float64(stats.TotalBytes))
	m.UsedBytes.WithLabelValues(volumeType, tablespace).Set(float64(stats.UsedBytes))
	m.AvailableBytes.WithLabelValues(volumeType, tablespace).Set(float64(stats.AvailableBytes))
	m.PercentUsed.WithLabelValues(volumeType, tablespace).Set(float64(stats.PercentUsed))
	m.InodesTotal.WithLabelValues(volumeType, tablespace).Set(float64(stats.InodesTotal))
	m.InodesUsed.WithLabelValues(volumeType, tablespace).Set(float64(stats.InodesUsed))
	m.InodesFree.WithLabelValues(volumeType, tablespace).Set(float64(stats.InodesFree))
}
