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

// Package timeouts contains the timeouts for the E2E test suite
package timeouts

import (
	"encoding/json"
	"fmt"
	"os"
)

// TestTimeoutsEnvVar is the environment variable where specific timeouts can be
// set for the E2E test suite
const TestTimeoutsEnvVar = "TEST_TIMEOUTS"

// Timeout represents an event whose time we want to limit in the test suite
type Timeout string

// the events we're setting timeouts for
// NOTE: the text representation will be used as the fields in the JSON representation
// of the timeout object passed to the ginkgo command as an environment variable
const (
	Failover                  Timeout = "failover"
	NamespaceCreation         Timeout = "namespaceCreation"
	ClusterIsReady            Timeout = "clusterIsReady"
	ClusterIsReadyQuick       Timeout = "clusterIsReadyQuick"
	ClusterIsReadySlow        Timeout = "clusterIsReadySlow"
	NewPrimaryAfterSwitchover Timeout = "newPrimaryAfterSwitchover"
	NewPrimaryAfterFailover   Timeout = "newPrimaryAfterFailover"
	NewTargetOnFailover       Timeout = "newTargetOnFailover"
	PodRollout                Timeout = "podRollout"
	OperatorIsReady           Timeout = "operatorIsReady"
	LargeObject               Timeout = "largeObject"
	WalsInMinio               Timeout = "walsInMinio"
	MinioInstallation         Timeout = "minioInstallation"
	BackupIsReady             Timeout = "backupIsReady"
	DrainNode                 Timeout = "drainNode"
	VolumeSnapshotIsReady     Timeout = "volumeSnapshotIsReady"
	Short                     Timeout = "short"
	ManagedServices           Timeout = "managedServices"
	DiskFill                  Timeout = "diskFill"

	// AKS-specific timeouts for Azure CSI operations which can be slow
	AKSVolumeAttach       Timeout = "aksVolumeAttach"
	AKSVolumeDetach       Timeout = "aksVolumeDetach"
	AKSVolumeResize       Timeout = "aksVolumeResize"
	AKSPodReschedule      Timeout = "aksPodReschedule"
	AKSStorageProvisioned Timeout = "aksStorageProvisioned"

	// Standard polling interval for AKS operations (30 seconds)
	AKSPollingInterval Timeout = "aksPollingInterval"

	// AKS backup timeout - backups during volume resize can take much longer on Azure
	AKSBackupIsReady Timeout = "aksBackupIsReady"

	// StorageSizingDetection is the time to wait for the operator to detect a storage sizing need
	StorageSizingDetection Timeout = "storageSizingDetection"
	// StorageSizingPolling is the polling interval for storage sizing status checks
	StorageSizingPolling Timeout = "storageSizingPolling"
)

// DefaultTestTimeouts contains the default timeout in seconds for various events
var DefaultTestTimeouts = map[Timeout]int{
	Failover:                  240,
	NamespaceCreation:         30,
	ClusterIsReady:            600,
	ClusterIsReadyQuick:       300,
	ClusterIsReadySlow:        1800, // 30 min for AKS volume operations (resize + reattach after pod restart)
	NewPrimaryAfterSwitchover: 45,
	NewPrimaryAfterFailover:   30,
	NewTargetOnFailover:       120,
	PodRollout:                180,
	OperatorIsReady:           120,
	LargeObject:               300,
	WalsInMinio:               60,
	MinioInstallation:         300,
	BackupIsReady:             180,
	DrainNode:                 1800, // 30 min for AKS drain + volume operations (detach + reattach)
	VolumeSnapshotIsReady:     300,
	Short:                     5,
	ManagedServices:           30,
	DiskFill:                  600,

	// AKS-specific timeouts - Azure disk CSI operations are slower than other providers
	AKSVolumeAttach:       600,  // 10 min - Azure disk attach can take 5-8 min
	AKSVolumeDetach:       600,  // 10 min - Azure disk detach can be slow
	AKSVolumeResize:       2400, // 40 min - Online resize can take 20-30 min on Azure with heavy I/O
	AKSPodReschedule:      1200, // 20 min - Pod reschedule with volume reattach
	AKSStorageProvisioned: 600,  // 10 min - Initial PVC provisioning on AKS
	AKSPollingInterval:    30,   // 30 sec - Standard polling for AKS operations
	AKSBackupIsReady:      600,  // 10 min - Backups during volume resize can be slow on AKS

	// Storage sizing timeouts
	StorageSizingDetection: 300, // 5 min - Time for operator to detect sizing need
	StorageSizingPolling:   10,  // 10 sec - Polling interval for sizing status checks
}

// Timeouts returns the map of timeouts, where each event gets the timeout specified
// in the `TEST_TIMEOUTS` environment variable, or if not specified, takes the default
// value
func Timeouts() (map[Timeout]int, error) {
	if timeoutsEnv, exists := os.LookupEnv(TestTimeoutsEnvVar); exists {
		var timeouts map[Timeout]int
		err := json.Unmarshal([]byte(timeoutsEnv), &timeouts)
		if err != nil {
			return map[Timeout]int{},
				fmt.Errorf("TEST_TIMEOUTS env variable is not valid: %v", err)
		}
		for k, def := range DefaultTestTimeouts {
			val, found := timeouts[k]
			if found {
				timeouts[k] = val
			} else {
				timeouts[k] = def
			}
		}
		return timeouts, nil
	}

	return DefaultTestTimeouts, nil
}
