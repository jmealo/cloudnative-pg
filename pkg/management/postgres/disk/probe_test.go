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
	"os"
	"testing"
)

func TestGetStatsReturnsValidData(t *testing.T) {
	// Use temp directory for testing
	tmpDir, err := os.MkdirTemp("", "disk-probe-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	probe := NewProbeWithPaths(tmpDir, "", "")
	ctx := context.Background()

	stats, err := probe.GetDataStats(ctx)
	if err != nil {
		t.Fatalf("GetDataStats failed: %v", err)
	}

	if stats == nil {
		t.Fatal("stats should not be nil")
	}

	if stats.TotalBytes == 0 {
		t.Error("TotalBytes should be > 0")
	}

	if stats.PercentUsed < 0 || stats.PercentUsed > 100 {
		t.Errorf("PercentUsed should be 0-100, got %d", stats.PercentUsed)
	}

	if stats.Path != tmpDir {
		t.Errorf("Path should be %s, got %s", tmpDir, stats.Path)
	}
}

func TestGetWALStatsReturnsNilForSingleVolume(t *testing.T) {
	probe := NewProbeWithPaths("/tmp", "", "")
	ctx := context.Background()

	stats, err := probe.GetWALStats(ctx, false) // separateWAL = false
	if err != nil {
		t.Fatalf("GetWALStats failed: %v", err)
	}

	if stats != nil {
		t.Error("WAL stats should be nil for single volume")
	}
}

func TestGetWALStatsReturnsDataForSeparateVolume(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "disk-probe-wal-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	probe := NewProbeWithPaths("/tmp", tmpDir, "")
	ctx := context.Background()

	stats, err := probe.GetWALStats(ctx, true) // separateWAL = true
	if err != nil {
		t.Fatalf("GetWALStats failed: %v", err)
	}

	if stats == nil {
		t.Fatal("WAL stats should not be nil for separate volume")
	}

	if stats.TotalBytes == 0 {
		t.Error("TotalBytes should be > 0")
	}
}
