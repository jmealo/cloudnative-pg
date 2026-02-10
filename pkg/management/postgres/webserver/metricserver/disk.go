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
	"errors"
	"time"

	"github.com/cloudnative-pg/machinery/pkg/log"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/resource"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/internal/management/cache"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/management/postgres"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/management/postgres/webserver/client/local"
	postgresstatus "github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"
)

type diskCollector struct {
	instance   *postgres.Instance
	getCluster func() (*apiv1.Cluster, error)

	// Disk usage metrics
	totalBytes     *prometheus.GaugeVec
	usedBytes      *prometheus.GaugeVec
	availableBytes *prometheus.GaugeVec
	percentUsed    *prometheus.GaugeVec

	// Dynamic storage metrics
	targetSizeBytes          *prometheus.GaugeVec
	actualSizeBytes          *prometheus.GaugeVec
	effectiveSizeBytes        *prometheus.GaugeVec
	state                    *prometheus.GaugeVec
	budgetTotal              *prometheus.GaugeVec
	budgetUsed               *prometheus.GaugeVec
	budgetEmergencyReserved  *prometheus.GaugeVec
	nextWindowSeconds        *prometheus.GaugeVec
}

func newDiskCollector(instance *postgres.Instance) *diskCollector {
	subsystem := "disk"
	dynamicSubsystem := "dynamic_storage"
	clusterGetter := local.NewClient().Cache().GetCluster

	return &diskCollector{
		instance:   instance,
		getCluster: clusterGetter,

		totalBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Subsystem: subsystem,
			Name:      "total_bytes",
			Help:      "Total filesystem size in bytes",
		}, []string{"volume_type", "instance"}),

		usedBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Subsystem: subsystem,
			Name:      "used_bytes",
			Help:      "Used bytes on filesystem",
		}, []string{"volume_type", "instance"}),

		availableBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Subsystem: subsystem,
			Name:      "available_bytes",
			Help:      "Available bytes on filesystem",
		}, []string{"volume_type", "instance"}),

		percentUsed: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Subsystem: subsystem,
			Name:      "percent_used",
			Help:      "Percentage of filesystem used",
		}, []string{"volume_type", "instance"}),

		targetSizeBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Subsystem: dynamicSubsystem,
			Name:      "target_size_bytes",
			Help:      "Target size from dynamic config",
		}, []string{"volume_type", "tablespace"}),

		actualSizeBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Subsystem: dynamicSubsystem,
			Name:      "actual_size_bytes",
			Help:      "Actual PVC size per instance",
		}, []string{"volume_type", "tablespace", "instance"}),

		effectiveSizeBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Subsystem: dynamicSubsystem,
			Name:      "effective_size_bytes",
			Help:      "Current effective size for new replicas",
		}, []string{"volume_type", "tablespace"}),

		state: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Subsystem: dynamicSubsystem,
			Name:      "state",
			Help:      "1 for current state, 0 for others",
		}, []string{"volume_type", "tablespace", "state"}),

		budgetTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Subsystem: dynamicSubsystem,
			Name:      "budget_total",
			Help:      "Total daily operations budget",
		}, []string{"volume_type"}),

		budgetUsed: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Subsystem: dynamicSubsystem,
			Name:      "budget_used",
			Help:      "Operations used in last 24h",
		}, []string{"volume_type"}),

		budgetEmergencyReserved: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Subsystem: dynamicSubsystem,
			Name:      "budget_emergency_reserved",
			Help:      "Emergency reserve remaining",
		}, []string{"volume_type"}),

		nextWindowSeconds: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: PrometheusNamespace,
			Subsystem: dynamicSubsystem,
			Name:      "next_window_seconds",
			Help:      "Seconds until next maintenance window",
		}, []string{"volume_type"}),
	}
}

func (d *diskCollector) Name() string {
	return "diskCollector"
}

func (d *diskCollector) Describe(ch chan<- *prometheus.Desc) {
	d.totalBytes.Describe(ch)
	d.usedBytes.Describe(ch)
	d.availableBytes.Describe(ch)
	d.percentUsed.Describe(ch)
	d.targetSizeBytes.Describe(ch)
	d.actualSizeBytes.Describe(ch)
	d.effectiveSizeBytes.Describe(ch)
	d.state.Describe(ch)
	d.budgetTotal.Describe(ch)
	d.budgetUsed.Describe(ch)
	d.budgetEmergencyReserved.Describe(ch)
	d.nextWindowSeconds.Describe(ch)
}

func (d *diskCollector) Collect(ch chan<- prometheus.Metric) {
	d.collectDiskUsage(ch)
	d.collectDynamicStorage(ch)
}

func (d *diskCollector) collectDiskUsage(ch chan<- prometheus.Metric) {
	status, err := d.instance.GetStatus()
	if err != nil {
		return
	}

	podName := d.instance.GetPodName()

	// Data volume
	if status.DiskStatus != nil {
		d.recordDiskStatus(ch, "data", podName, status.DiskStatus)
	}

	// WAL volume
	if status.WALDiskStatus != nil {
		d.recordDiskStatus(ch, "wal", podName, status.WALDiskStatus)
	}

	// Tablespaces
	for tsName, tsStatus := range status.TablespaceDiskStatus {
		d.recordDiskStatus(ch, "tablespace:"+tsName, podName, tsStatus)
	}
}

func (d *diskCollector) recordDiskStatus(ch chan<- prometheus.Metric, volumeType, instance string, s *postgresstatus.DiskStatus) {
	d.totalBytes.WithLabelValues(volumeType, instance).Set(float64(s.TotalBytes))
	d.usedBytes.WithLabelValues(volumeType, instance).Set(float64(s.UsedBytes))
	d.availableBytes.WithLabelValues(volumeType, instance).Set(float64(s.AvailableBytes))
	d.percentUsed.WithLabelValues(volumeType, instance).Set(s.PercentUsed)

	d.totalBytes.Collect(ch)
	d.usedBytes.Collect(ch)
	d.availableBytes.Collect(ch)
	d.percentUsed.Collect(ch)
}

func (d *diskCollector) collectDynamicStorage(ch chan<- prometheus.Metric) {
	cluster, err := d.getCluster()
	if err != nil {
		if !errors.Is(err, cache.ErrCacheMiss) {
			log.Error(err, "error while retrieving cluster cache object for disk metrics")
		}
		return
	}

	if cluster.Status.StorageSizing == nil {
		return
	}

	// Data volume sizing
	if cluster.Status.StorageSizing.Data != nil {
		d.recordVolumeSizing(ch, "data", "", cluster.Status.StorageSizing.Data)
	}

	// WAL volume sizing
	if cluster.Status.StorageSizing.WAL != nil {
		d.recordVolumeSizing(ch, "wal", "", cluster.Status.StorageSizing.WAL)
	}

	// Tablespace sizing
	for tsName, tsStatus := range cluster.Status.StorageSizing.Tablespaces {
		d.recordVolumeSizing(ch, "tablespace", tsName, tsStatus)
	}
}

func (d *diskCollector) recordVolumeSizing(
	ch chan<- prometheus.Metric,
	volumeType, tablespace string,
	s *apiv1.VolumeSizingStatus,
) {
	// Target and Effective sizes
	if s.TargetSize != "" {
		if val, err := d.parseQuantity(s.TargetSize); err == nil {
			d.targetSizeBytes.WithLabelValues(volumeType, tablespace).Set(val)
		}
	}
	if s.EffectiveSize != "" {
		if val, err := d.parseQuantity(s.EffectiveSize); err == nil {
			d.effectiveSizeBytes.WithLabelValues(volumeType, tablespace).Set(val)
		}
	}

	// Actual sizes per instance
	for instanceName, sizeStr := range s.ActualSizes {
		if val, err := d.parseQuantity(sizeStr); err == nil {
			d.actualSizeBytes.WithLabelValues(volumeType, tablespace, instanceName).Set(val)
		}
	}

	// State (1 for current state, 0 for others)
	states := []string{"Balanced", "NeedsGrow", "Emergency", "PendingGrowth", "Resizing"}
	for _, state := range states {
		val := 0.0
		if s.State == state {
			val = 1.0
		}
		d.state.WithLabelValues(volumeType, tablespace, state).Set(val)
	}

	// Budget
	if s.Budget != nil {
		d.budgetUsed.WithLabelValues(volumeType).Set(float64(s.Budget.ActionsLast24h))
		d.budgetEmergencyReserved.WithLabelValues(volumeType).Set(float64(s.Budget.AvailableForEmergency))
		// Available for planned is also useful
		d.budgetTotal.WithLabelValues(volumeType).Set(float64(s.Budget.AvailableForPlanned + s.Budget.ActionsLast24h))
	}

	// Next window
	if s.NextMaintenanceWindow != nil {
		d.nextWindowSeconds.WithLabelValues(volumeType).Set(time.Until(s.NextMaintenanceWindow.Time).Seconds())
	} else {
		d.nextWindowSeconds.WithLabelValues(volumeType).Set(0)
	}

	d.targetSizeBytes.Collect(ch)
	d.effectiveSizeBytes.Collect(ch)
	d.actualSizeBytes.Collect(ch)
	d.state.Collect(ch)
	d.budgetTotal.Collect(ch)
	d.budgetUsed.Collect(ch)
	d.budgetEmergencyReserved.Collect(ch)
	d.nextWindowSeconds.Collect(ch)
}

func (d *diskCollector) parseQuantity(q string) (float64, error) {
	qty, err := resource.ParseQuantity(q)
	if err != nil {
		return 0, err
	}
	return float64(qty.Value()), nil
}
