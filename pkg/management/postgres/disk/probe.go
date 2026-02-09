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

// Package disk provides utilities for probing disk space usage.
package disk

// Status represents filesystem statistics for a mount point.
type Status struct {
	// TotalBytes is the total size of the filesystem in bytes.
	TotalBytes uint64 `json:"totalBytes"`

	// UsedBytes is the number of bytes used on the filesystem.
	UsedBytes uint64 `json:"usedBytes"`

	// AvailableBytes is the number of bytes available to non-root users.
	AvailableBytes uint64 `json:"availableBytes"`

	// PercentUsed is the percentage of the filesystem that is used.
	PercentUsed float64 `json:"percentUsed"`
}
