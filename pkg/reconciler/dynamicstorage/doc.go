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

// Package dynamicstorage implements the dynamic storage sizing reconciler.
// This package provides automatic storage management within user-defined bounds
// (request/limit) while maintaining a target free-space buffer.
//
// Key features:
// - Monitors disk usage from instance status
// - Calculates target size based on usage + buffer percentage
// - Emergency growth when critical threshold is reached
// - Scheduled growth during maintenance windows
// - Rate limiting via daily action budgets
// - Updates cluster status with sizing information
package dynamicstorage
