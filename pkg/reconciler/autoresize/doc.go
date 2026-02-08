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

// Package autoresize contains the logic for automatic PVC resizing based on
// disk usage thresholds. It monitors volume usage and triggers PVC expansion
// when usage exceeds configured thresholds, with WAL safety checks to prevent
// masking archive or replication issues.
package autoresize
