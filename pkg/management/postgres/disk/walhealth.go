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
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/cloudnative-pg/cloudnative-pg/pkg/specs"
)

// WALHealthStatus contains WAL health information for auto-resize decisions
type WALHealthStatus struct {
	// ArchiveHealthy indicates WAL archiving is working
	ArchiveHealthy bool
	// PendingArchiveFiles is the count of files awaiting archive
	PendingArchiveFiles int
	// LastArchiveSuccess is when the last successful archive occurred
	LastArchiveSuccess *time.Time
	// LastArchiveFailure is when the last archive failure occurred
	LastArchiveFailure *time.Time
	// InactiveSlots lists inactive replication slots
	InactiveSlots []SlotInfo
	// TotalSlotRetentionBytes is total WAL retained by inactive slots
	TotalSlotRetentionBytes int64
}

// SlotInfo contains replication slot information
type SlotInfo struct {
	Name          string
	Active        bool
	RetainedBytes int64
	RestartLSN    string
}

// WALHealthChecker evaluates WAL health for auto-resize decisions
type WALHealthChecker struct {
	archiveStatusPath string
}

// NewWALHealthChecker creates a new WAL health checker using standard paths
func NewWALHealthChecker() *WALHealthChecker {
	return &WALHealthChecker{
		archiveStatusPath: filepath.Join(specs.PgWalPath, "archive_status"),
	}
}

// NewWALHealthCheckerWithPath creates a new WAL health checker with custom path (for testing)
func NewWALHealthCheckerWithPath(archiveStatusPath string) *WALHealthChecker {
	return &WALHealthChecker{
		archiveStatusPath: archiveStatusPath,
	}
}

// walFileRegex matches WAL file names with .ready extension
var walFileRegex = regexp.MustCompile(`^[0-9A-F]{24}\.ready$`)

// CountPendingArchive counts WAL files awaiting archive
func (h *WALHealthChecker) CountPendingArchive() (int, error) {
	entries, err := os.ReadDir(h.archiveStatusPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	count := 0
	for _, entry := range entries {
		if walFileRegex.MatchString(entry.Name()) {
			count++
		}
	}
	return count, nil
}

// Check evaluates current WAL health using database connection
func (h *WALHealthChecker) Check(ctx context.Context, db *sql.DB) (*WALHealthStatus, error) {
	status := &WALHealthStatus{
		ArchiveHealthy: true,
	}

	// Count pending archive files
	ready, err := h.CountPendingArchive()
	if err != nil {
		return nil, err
	}
	status.PendingArchiveFiles = ready

	// Consider archive unhealthy if too many files pending
	if ready > 10 {
		status.ArchiveHealthy = false
	}

	// Check archive timestamps from pg_stat_archiver
	if db != nil {
		if err := h.checkArchiveStats(ctx, db, status); err != nil {
			return nil, err
		}

		// Check replication slots
		slots, err := h.checkReplicationSlots(ctx, db)
		if err != nil {
			return nil, err
		}

		for _, slot := range slots {
			if !slot.Active {
				status.InactiveSlots = append(status.InactiveSlots, slot)
				status.TotalSlotRetentionBytes += slot.RetainedBytes
			}
		}
	}

	return status, nil
}

func (h *WALHealthChecker) checkArchiveStats(ctx context.Context, db *sql.DB, status *WALHealthStatus) error {
	row := db.QueryRowContext(ctx, `
		SELECT
			last_archived_time,
			last_failed_time,
			failed_count
		FROM pg_stat_archiver
	`)

	var lastArchived, lastFailed sql.NullTime
	var failedCount int64

	if err := row.Scan(&lastArchived, &lastFailed, &failedCount); err != nil {
		return err
	}

	if lastArchived.Valid {
		status.LastArchiveSuccess = &lastArchived.Time
	}
	if lastFailed.Valid {
		status.LastArchiveFailure = &lastFailed.Time
		// If last failure is more recent than last success, archive is unhealthy
		if status.LastArchiveSuccess == nil || lastFailed.Time.After(*status.LastArchiveSuccess) {
			status.ArchiveHealthy = false
		}
	}

	return nil
}

func (h *WALHealthChecker) checkReplicationSlots(ctx context.Context, db *sql.DB) ([]SlotInfo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			slot_name,
			active,
			restart_lsn,
			pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) as retained_bytes
		FROM pg_replication_slots
		WHERE slot_type = 'physical'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var slots []SlotInfo
	for rows.Next() {
		var slot SlotInfo
		var restartLSN sql.NullString
		var retainedBytes sql.NullInt64

		if err := rows.Scan(&slot.Name, &slot.Active, &restartLSN, &retainedBytes); err != nil {
			return nil, err
		}

		if restartLSN.Valid {
			slot.RestartLSN = restartLSN.String
		}
		if retainedBytes.Valid {
			slot.RetainedBytes = retainedBytes.Int64
		}

		slots = append(slots, slot)
	}

	return slots, rows.Err()
}
