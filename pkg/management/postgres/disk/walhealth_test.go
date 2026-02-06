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
	"os"
	"path/filepath"
	"testing"
)

func TestCountPendingArchive(t *testing.T) {
	// Create temp archive_status directory
	tmpDir, err := os.MkdirTemp("", "wal-health-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	archiveStatusPath := tmpDir

	// Create some .ready files (WAL files awaiting archive)
	readyFiles := []string{
		"000000010000000000000001.ready",
		"000000010000000000000002.ready",
		"000000010000000000000003.ready",
		"000000010000000000000004.ready",
		"000000010000000000000005.ready",
	}

	for _, fileName := range readyFiles {
		filePath := filepath.Join(archiveStatusPath, fileName)
		if err := os.WriteFile(filePath, []byte{}, 0644); err != nil {
			t.Fatalf("failed to create ready file: %v", err)
		}
	}

	checker := NewWALHealthCheckerWithPath(archiveStatusPath)
	count, err := checker.CountPendingArchive()
	if err != nil {
		t.Fatalf("CountPendingArchive failed: %v", err)
	}

	if count != 5 {
		t.Errorf("expected 5 pending files, got %d", count)
	}
}

func TestCountPendingArchiveEmptyDir(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "wal-health-test")
	defer os.RemoveAll(tmpDir)

	checker := NewWALHealthCheckerWithPath(tmpDir)
	count, err := checker.CountPendingArchive()
	if err != nil {
		t.Fatalf("CountPendingArchive failed: %v", err)
	}

	if count != 0 {
		t.Errorf("expected 0 pending files, got %d", count)
	}
}

func TestCountPendingArchiveNonExistentDir(t *testing.T) {
	checker := NewWALHealthCheckerWithPath("/nonexistent/path/archive_status")
	count, err := checker.CountPendingArchive()
	if err != nil {
		t.Fatalf("CountPendingArchive should not error for non-existent dir: %v", err)
	}

	if count != 0 {
		t.Errorf("expected 0 pending files for non-existent dir, got %d", count)
	}
}

func TestCountPendingArchiveIgnoresNonReadyFiles(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "wal-health-test")
	defer os.RemoveAll(tmpDir)

	// Create mixed files
	files := []string{
		"000000010000000000000001.ready", // Should count
		"000000010000000000000002.done",  // Should NOT count
		"000000010000000000000003.ready", // Should count
		"some_other_file.txt",            // Should NOT count
	}

	for _, fileName := range files {
		filePath := filepath.Join(tmpDir, fileName)
		if err := os.WriteFile(filePath, []byte{}, 0644); err != nil {
			t.Fatalf("failed to create file: %v", err)
		}
	}

	checker := NewWALHealthCheckerWithPath(tmpDir)
	count, err := checker.CountPendingArchive()
	if err != nil {
		t.Fatalf("CountPendingArchive failed: %v", err)
	}

	if count != 2 {
		t.Errorf("expected 2 pending files (only .ready), got %d", count)
	}
}
