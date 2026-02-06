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

// Package disk provides disk space monitoring functionality
package disk

import (
	"context"
	"syscall"

	"github.com/cloudnative-pg/cloudnative-pg/pkg/specs"
)

// VolumeStats contains filesystem statistics for a volume
type VolumeStats struct {
	Path           string
	TotalBytes     uint64
	UsedBytes      uint64
	AvailableBytes uint64
	PercentUsed    int
	InodesTotal    uint64
	InodesUsed     uint64
	InodesFree     uint64
}

// Probe provides disk statistics for CNPG volumes
type Probe struct {
	dataPath       string
	walPath        string
	tablespacePath string
}

// NewProbe creates a new disk probe for the standard CNPG paths
func NewProbe() *Probe {
	return &Probe{
		dataPath:       specs.PgDataPath,
		walPath:        specs.PgWalVolumePath,
		tablespacePath: "/var/lib/postgresql/tablespaces",
	}
}

// NewProbeWithPaths creates a new disk probe with custom paths (for testing)
func NewProbeWithPaths(dataPath, walPath, tablespacePath string) *Probe {
	return &Probe{
		dataPath:       dataPath,
		walPath:        walPath,
		tablespacePath: tablespacePath,
	}
}

// GetDataStats returns filesystem stats for the PGDATA volume
func (p *Probe) GetDataStats(_ context.Context) (*VolumeStats, error) {
	return p.getStats(p.dataPath)
}

// GetWALStats returns filesystem stats for the WAL volume
// Returns nil if WAL is on the same volume as PGDATA
func (p *Probe) GetWALStats(_ context.Context, separateWAL bool) (*VolumeStats, error) {
	if !separateWAL {
		return nil, nil
	}
	return p.getStats(p.walPath)
}

// GetTablespaceStats returns filesystem stats for a tablespace volume
func (p *Probe) GetTablespaceStats(_ context.Context, tablespaceName string) (*VolumeStats, error) {
	path := specs.MountForTablespace(tablespaceName)
	return p.getStats(path)
}

func (p *Probe) getStats(path string) (*VolumeStats, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, err
	}

	// Calculate bytes - use Bsize for block size
	blockSize := uint64(stat.Bsize)
	totalBytes := stat.Blocks * blockSize
	freeBytes := stat.Bavail * blockSize
	usedBytes := totalBytes - (stat.Bfree * blockSize)

	var percentUsed int
	if totalBytes > 0 {
		// Use float64 to avoid potential overflow on multiplication
		percentUsed = int(float64(usedBytes) / float64(totalBytes) * 100)
	}

	return &VolumeStats{
		Path:           path,
		TotalBytes:     totalBytes,
		UsedBytes:      usedBytes,
		AvailableBytes: freeBytes,
		PercentUsed:    percentUsed,
		InodesTotal:    stat.Files,
		InodesFree:     stat.Ffree,
		InodesUsed:     stat.Files - stat.Ffree,
	}, nil
}
