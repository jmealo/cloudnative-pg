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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("AutoResizeConfiguration", func() {
	It("can be created with default values", func() {
		config := &AutoResizeConfiguration{
			Enabled: true,
		}
		Expect(config.Enabled).To(BeTrue())
		// Threshold defaults to 80 via kubebuilder marker
		// Increase defaults to "20%" via kubebuilder marker
	})
})

var _ = Describe("WALSafetyPolicy", func() {
	It("can be created with default values", func() {
		policy := &WALSafetyPolicy{}
		Expect(policy).ToNot(BeNil())
		// RequireArchiveHealthy defaults to true via kubebuilder marker
	})
})

var _ = Describe("StorageConfiguration", func() {
	It("supports AutoResize field", func() {
		storage := StorageConfiguration{
			Size: "10Gi",
			AutoResize: &AutoResizeConfiguration{
				Enabled:   true,
				Threshold: 80,
				Increase:  "20%",
				MaxSize:   "100Gi",
			},
		}

		Expect(storage.AutoResize).ToNot(BeNil())
		Expect(storage.AutoResize.Enabled).To(BeTrue())
		Expect(storage.AutoResize.Threshold).To(Equal(80))
	})
})

var _ = Describe("ClusterDiskStatus", func() {
	It("can hold instance disk status", func() {
		status := &ClusterDiskStatus{
			Instances: []InstanceDiskStatus{
				{
					PodName: "cluster-1",
					Data: &VolumeDiskStatus{
						TotalBytes:     100 * 1024 * 1024 * 1024,
						UsedBytes:      80 * 1024 * 1024 * 1024,
						AvailableBytes: 20 * 1024 * 1024 * 1024,
						PercentUsed:    80,
					},
				},
			},
		}

		Expect(status.Instances).To(HaveLen(1))
		Expect(status.Instances[0].Data.PercentUsed).To(Equal(80))
	})
})

var _ = Describe("ClusterStatus", func() {
	It("supports DiskStatus field", func() {
		cluster := &Cluster{
			Status: ClusterStatus{
				DiskStatus: &ClusterDiskStatus{
					Instances: []InstanceDiskStatus{
						{PodName: "test-1"},
					},
				},
			},
		}

		Expect(cluster.Status.DiskStatus).ToNot(BeNil())
	})
})
