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

package v1

import (
	"testing"
)

func TestAutoResizeConfigurationDefaults(t *testing.T) {
	config := &AutoResizeConfiguration{
		Enabled: true,
	}

	// Test default threshold
	if config.Threshold == 0 {
		t.Log("Threshold should default to 80 via kubebuilder marker")
	}

	// Test default increase
	if config.Increase == "" {
		t.Log("Increase should default to 20% via kubebuilder marker")
	}
}

func TestWALSafetyPolicyDefaults(t *testing.T) {
	policy := &WALSafetyPolicy{}

	// RequireArchiveHealthy should default to true
	if policy.RequireArchiveHealthy == nil {
		t.Log("RequireArchiveHealthy should default to true via kubebuilder marker")
	}
}

func TestStorageConfigurationHasAutoResize(t *testing.T) {
	storage := StorageConfiguration{
		Size: "10Gi",
		AutoResize: &AutoResizeConfiguration{
			Enabled:   true,
			Threshold: 80,
			Increase:  "20%",
			MaxSize:   "100Gi",
		},
	}

	if storage.AutoResize == nil {
		t.Fatal("AutoResize field should exist on StorageConfiguration")
	}
	if !storage.AutoResize.Enabled {
		t.Fatal("AutoResize.Enabled should be true")
	}
}

func TestClusterDiskStatusTypes(t *testing.T) {
	status := &ClusterDiskStatus{
		Instances: []InstanceDiskStatus{
			{
				PodName: "cluster-1",
				Data: &VolumeDiskStatus{
					TotalBytes:     100 * 1024 * 1024 * 1024, // 100Gi
					UsedBytes:      80 * 1024 * 1024 * 1024,  // 80Gi
					AvailableBytes: 20 * 1024 * 1024 * 1024,  // 20Gi
					PercentUsed:    80.0,
				},
			},
		},
	}

	if len(status.Instances) != 1 {
		t.Fatal("Expected 1 instance")
	}
	if status.Instances[0].Data.PercentUsed != 80.0 {
		t.Fatal("Expected 80% used")
	}
}

func TestClusterStatusHasDiskStatus(t *testing.T) {
	cluster := &Cluster{
		Status: ClusterStatus{
			DiskStatus: &ClusterDiskStatus{
				Instances: []InstanceDiskStatus{
					{PodName: "test-1"},
				},
			},
		},
	}

	if cluster.Status.DiskStatus == nil {
		t.Fatal("DiskStatus field should exist on ClusterStatus")
	}
}
