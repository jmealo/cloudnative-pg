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
	"database/sql"

	"github.com/cloudnative-pg/machinery/pkg/log"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/cloudnative-pg/cloudnative-pg/pkg/management/postgres"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/management/postgres/disk"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/management/postgres/wal"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/management/postgres/webserver/client/local"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/specs"
)

// DiskMetrics contains the Prometheus metric descriptors for disk usage.
type DiskMetrics struct {
	TotalBytes            *prometheus.GaugeVec
	UsedBytes             *prometheus.GaugeVec
	AvailableBytes        *prometheus.GaugeVec
	PercentUsed           *prometheus.GaugeVec
	InodesTotal           *prometheus.GaugeVec
	InodesUsed            *prometheus.GaugeVec
	InodesFree            *prometheus.GaugeVec
	AtLimit               *prometheus.GaugeVec
	ResizeBlocked         *prometheus.GaugeVec
	ResizesTotal          *prometheus.CounterVec
	ResizeBudgetRemain    *prometheus.GaugeVec
	WALArchiveHealthy     prometheus.Gauge
	WALPendingFiles       prometheus.Gauge
	WALInactiveSlots      prometheus.Gauge
	WALSlotRetentionBytes *prometheus.GaugeVec
}

// newDiskMetrics returns the disk-related Prometheus metrics.
func newDiskMetrics() *DiskMetrics {
	return &DiskMetrics{
		TotalBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "disk_total_bytes",
			Help:      "Total capacity of the volume in bytes.",
		}, []string{"volume_type", "tablespace"}),
		UsedBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "disk_used_bytes",
			Help:      "Used space on the volume in bytes.",
		}, []string{"volume_type", "tablespace"}),
		AvailableBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "disk_available_bytes",
			Help:      "Available space on the volume in bytes (non-root).",
		}, []string{"volume_type", "tablespace"}),
		PercentUsed: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "disk_percent_used",
			Help:      "Percentage of the volume in use (0-100).",
		}, []string{"volume_type", "tablespace"}),
		InodesTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "disk_inodes_total",
			Help:      "Total number of inodes on the volume.",
		}, []string{"volume_type", "tablespace"}),
		InodesUsed: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "disk_inodes_used",
			Help:      "Number of inodes in use on the volume.",
		}, []string{"volume_type", "tablespace"}),
		InodesFree: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "disk_inodes_free",
			Help:      "Number of free inodes on the volume.",
		}, []string{"volume_type", "tablespace"}),
		AtLimit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "disk_at_limit",
			Help:      "1 if the volume is at its configured expansion.limit, 0 otherwise.",
		}, []string{"volume_type", "tablespace"}),
		ResizeBlocked: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "disk_resize_blocked",
			Help:      "1 if auto-resize is blocked, with reason label.",
		}, []string{"volume_type", "tablespace", "reason"}),
		ResizesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: PrometheusNamespace,
			Name:      "disk_resizes_total",
			Help:      "Total number of auto-resize operations.",
		}, []string{"volume_type", "tablespace", "result"}),
		ResizeBudgetRemain: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "disk_resize_budget_remaining",
			Help:      "Number of remaining auto-resize operations in the current 24h budget.",
		}, []string{"volume_type", "tablespace"}),
		WALArchiveHealthy: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "wal_archive_healthy",
			Help:      "1 if WAL archiving is healthy (last_archived_time > last_failed_time), 0 otherwise.",
		}),
		WALPendingFiles: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "wal_pending_archive_files",
			Help:      "Number of WAL files pending archiving (.ready files in archive_status).",
		}),
		WALInactiveSlots: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "wal_inactive_slots",
			Help:      "Number of inactive physical replication slots.",
		}),
		WALSlotRetentionBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Name:      "wal_slot_retention_bytes",
			Help:      "WAL retention in bytes for inactive physical replication slots.",
		}, []string{"slot_name"}),
	}
}

// describeDiskMetrics describes all disk-related metrics.
func (dm *DiskMetrics) describe(ch chan<- *prometheus.Desc) {
	dm.TotalBytes.Describe(ch)
	dm.UsedBytes.Describe(ch)
	dm.AvailableBytes.Describe(ch)
	dm.PercentUsed.Describe(ch)
	dm.InodesTotal.Describe(ch)
	dm.InodesUsed.Describe(ch)
	dm.InodesFree.Describe(ch)
	dm.AtLimit.Describe(ch)
	dm.ResizeBlocked.Describe(ch)
	dm.ResizesTotal.Describe(ch)
	dm.ResizeBudgetRemain.Describe(ch)
	ch <- dm.WALArchiveHealthy.Desc()
	ch <- dm.WALPendingFiles.Desc()
	ch <- dm.WALInactiveSlots.Desc()
	dm.WALSlotRetentionBytes.Describe(ch)
}

// collectDiskMetrics sends all disk-related metrics to the channel.
func (dm *DiskMetrics) collect(ch chan<- prometheus.Metric) {
	dm.TotalBytes.Collect(ch)
	dm.UsedBytes.Collect(ch)
	dm.AvailableBytes.Collect(ch)
	dm.PercentUsed.Collect(ch)
	dm.InodesTotal.Collect(ch)
	dm.InodesUsed.Collect(ch)
	dm.InodesFree.Collect(ch)
	dm.AtLimit.Collect(ch)
	dm.ResizeBlocked.Collect(ch)
	dm.ResizesTotal.Collect(ch)
	dm.ResizeBudgetRemain.Collect(ch)
	ch <- dm.WALArchiveHealthy
	ch <- dm.WALPendingFiles
	ch <- dm.WALInactiveSlots
	dm.WALSlotRetentionBytes.Collect(ch)
}

// setVolumeStats sets the disk usage metrics for a single volume.
func (dm *DiskMetrics) setVolumeStats(result *disk.VolumeProbeResult) {
	volType := string(result.VolumeType)
	ts := result.Tablespace

	dm.TotalBytes.WithLabelValues(volType, ts).Set(float64(result.Stats.TotalBytes))
	dm.UsedBytes.WithLabelValues(volType, ts).Set(float64(result.Stats.UsedBytes))
	dm.AvailableBytes.WithLabelValues(volType, ts).Set(float64(result.Stats.AvailableBytes))
	dm.PercentUsed.WithLabelValues(volType, ts).Set(result.Stats.PercentUsed)
	dm.InodesTotal.WithLabelValues(volType, ts).Set(float64(result.Stats.InodesTotal))
	dm.InodesUsed.WithLabelValues(volType, ts).Set(float64(result.Stats.InodesUsed))
	dm.InodesFree.WithLabelValues(volType, ts).Set(float64(result.Stats.InodesFree))
}

// collectWALHealthMetrics queries WAL archive health and updates metrics.
func collectWALHealthMetrics(e *Exporter, db *sql.DB, isPrimary bool) {
	contextLogger := log.WithName("wal_health_metrics")

	getReadyWALCount := func() (int, error) {
		ready, _, err := postgres.GetWALArchiveCounters()
		return ready, err
	}

	checker := wal.NewHealthChecker(getReadyWALCount)
	status, err := checker.Check(db, isPrimary)
	if err != nil {
		contextLogger.Error(err, "failed to check WAL health")
		return
	}

	if status.ArchiveHealthy {
		e.Metrics.DiskMetrics.WALArchiveHealthy.Set(1)
	} else {
		e.Metrics.DiskMetrics.WALArchiveHealthy.Set(0)
	}

	e.Metrics.DiskMetrics.WALPendingFiles.Set(float64(status.PendingWALFiles))
	e.Metrics.DiskMetrics.WALInactiveSlots.Set(float64(len(status.InactiveSlots)))

	// Reset slot retention metrics before re-populating
	e.Metrics.DiskMetrics.WALSlotRetentionBytes.Reset()
	for _, slot := range status.InactiveSlots {
		e.Metrics.DiskMetrics.WALSlotRetentionBytes.WithLabelValues(slot.SlotName).Set(float64(slot.RetentionBytes))
	}
}

// collectDiskUsageMetrics probes all volumes (PGDATA, WAL, tablespaces) and updates metrics.
func collectDiskUsageMetrics(e *Exporter) {
	contextLogger := log.WithName("disk_metrics")
	probe := disk.NewProbe()

	// Probe PGDATA volume
	dataResult, err := probe.ProbeVolume(specs.PgDataPath, disk.VolumeTypeData, "")
	if err != nil {
		contextLogger.Error(err, "failed to probe PGDATA volume")
	} else {
		e.Metrics.DiskMetrics.setVolumeStats(dataResult)
	}

	// Probe WAL volume if it exists (separate from PGDATA)
	cluster, clusterErr := local.NewClient().Cache().GetCluster()
	if clusterErr == nil && cluster.ShouldCreateWalArchiveVolume() {
		walResult, err := probe.ProbeVolume(specs.PgWalVolumePath, disk.VolumeTypeWAL, "")
		if err != nil {
			contextLogger.Error(err, "failed to probe WAL volume")
		} else {
			e.Metrics.DiskMetrics.setVolumeStats(walResult)
		}
	}

	// Probe tablespace volumes
	if clusterErr == nil {
		for _, tbsConfig := range cluster.Spec.Tablespaces {
			tbsPath := specs.MountForTablespace(tbsConfig.Name)
			tbsResult, err := probe.ProbeVolume(tbsPath, disk.VolumeTypeTablespace, tbsConfig.Name)
			if err != nil {
				contextLogger.Error(err, "failed to probe tablespace volume",
					"tablespace", tbsConfig.Name)
			} else {
				e.Metrics.DiskMetrics.setVolumeStats(tbsResult)
			}
		}
	}
}
