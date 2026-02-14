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

	"github.com/cloudnative-pg/machinery/pkg/log"
	"github.com/robfig/cron"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
)

var (
	// DefaultMaintenanceSchedule is the default cron schedule for maintenance windows.
	// It uses the 6-field format: "second minute hour day-of-month month day-of-week"
	// This is consistent with ScheduledBackup cron format in CloudNativePG.
	DefaultMaintenanceSchedule = "0 0 3 * * *"

	// DefaultMaintenanceDuration is the default duration of maintenance windows.
	DefaultMaintenanceDuration = 2 * time.Hour

	// DefaultMaintenanceTimezone is the default timezone for maintenance windows.
	DefaultMaintenanceTimezone = "UTC"

	// cronParser is the parser for maintenance window schedules.
	// It uses the 6-field cron format (second, minute, hour, day of month, month, day of week).
	// This is consistent with ScheduledBackup cron format in CloudNativePG.
	cronParser = cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
)

// IsMaintenanceWindowOpen checks if the current time is within the maintenance window.
func IsMaintenanceWindowOpen(cfg *apiv1.StorageConfiguration) bool {
	if cfg == nil || cfg.MaintenanceWindow == nil {
		// No maintenance window configured = always open
		return true
	}

	schedule := cfg.MaintenanceWindow.Schedule
	if schedule == "" {
		schedule = DefaultMaintenanceSchedule
	}

	cronSchedule, err := cronParser.Parse(schedule)
	if err != nil {
		log.Warning("Failed to parse maintenance window cron schedule, treating window as closed",
			"schedule", schedule,
			"error", err)
		return false
	}

	// Get timezone
	loc := time.UTC
	if cfg.MaintenanceWindow.Timezone != "" {
		parsedLoc, err := time.LoadLocation(cfg.MaintenanceWindow.Timezone)
		if err != nil {
			log.Warning("Failed to parse maintenance window timezone, falling back to UTC",
				"timezone", cfg.MaintenanceWindow.Timezone,
				"error", err)
		} else {
			loc = parsedLoc
		}
	}

	// Get duration
	duration := DefaultMaintenanceDuration
	if cfg.MaintenanceWindow.Duration != "" {
		parsedDuration, err := time.ParseDuration(cfg.MaintenanceWindow.Duration)
		if err != nil {
			log.Warning("Failed to parse maintenance window duration, falling back to default",
				"duration", cfg.MaintenanceWindow.Duration,
				"default", DefaultMaintenanceDuration,
				"error", err)
		} else {
			duration = parsedDuration
		}
	}

	now := time.Now().In(loc)

	// Find the most recent window start before now
	// We look back up to 24 hours to find the last window
	windowStart := findMostRecentWindowStart(cronSchedule, now, 24*time.Hour)
	if windowStart.IsZero() {
		return false
	}

	windowEnd := windowStart.Add(duration)
	return now.After(windowStart) && now.Before(windowEnd)
}

// NextMaintenanceWindow returns the next maintenance window start time.
func NextMaintenanceWindow(cfg *apiv1.StorageConfiguration) *time.Time {
	if cfg == nil || cfg.MaintenanceWindow == nil {
		return nil
	}

	schedule := cfg.MaintenanceWindow.Schedule
	if schedule == "" {
		schedule = DefaultMaintenanceSchedule
	}

	cronSchedule, err := cronParser.Parse(schedule)
	if err != nil {
		log.Warning("Failed to parse maintenance window cron schedule for next window calculation",
			"schedule", schedule,
			"error", err)
		return nil
	}

	// Get timezone
	loc := time.UTC
	if cfg.MaintenanceWindow.Timezone != "" {
		parsedLoc, err := time.LoadLocation(cfg.MaintenanceWindow.Timezone)
		if err != nil {
			log.Warning("Failed to parse maintenance window timezone, falling back to UTC",
				"timezone", cfg.MaintenanceWindow.Timezone,
				"error", err)
		} else {
			loc = parsedLoc
		}
	}

	now := time.Now().In(loc)
	next := cronSchedule.Next(now)
	// If the schedule never produces a valid time (e.g., Feb 31st), return nil
	if next.IsZero() {
		return nil
	}
	return &next
}

// findMostRecentWindowStart finds the most recent window start within the lookback period.
func findMostRecentWindowStart(schedule cron.Schedule, now time.Time, lookback time.Duration) time.Time {
	// Start from lookback period ago
	checkTime := now.Add(-lookback)

	// Set a maximum number of iterations to prevent infinite loops
	// with invalid cron expressions (like Feb 31st)
	const maxIterations = 1000

	var lastStart time.Time
	for i := 0; i < maxIterations; i++ {
		nextStart := schedule.Next(checkTime)
		if nextStart.After(now) {
			break
		}
		// Guard against cron expressions that never advance
		if !nextStart.After(checkTime) {
			break
		}
		lastStart = nextStart
		checkTime = nextStart.Add(time.Minute) // Move past this start
	}

	return lastStart
}
