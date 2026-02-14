//go:build darwin

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

package disk

import (
	"syscall"
)

// Probe returns disk status for the given path using statfs.
// This implementation works on Unix-like systems (darwin, freebsd, etc).
func Probe(path string) (*Status, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, err
	}

	// Calculate sizes in bytes
	blockSize := uint64(stat.Bsize)
	total := stat.Blocks * blockSize
	free := stat.Bfree * blockSize
	available := stat.Bavail * blockSize
	used := total - free

	// Calculate percent used
	var percentUsed float64
	if total > 0 {
		percentUsed = float64(used) / float64(total) * 100
	}

	return &Status{
		TotalBytes:     total,
		UsedBytes:      used,
		AvailableBytes: available,
		PercentUsed:    percentUsed,
	}, nil
}
