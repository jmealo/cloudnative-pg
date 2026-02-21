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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Disk Probe", func() {
	var tmpDir string

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "disk-probe-test")
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		_ = os.RemoveAll(tmpDir)
	})

	Describe("GetDataStats", func() {
		It("returns valid filesystem statistics", func() {
			probe := NewProbeWithPaths(tmpDir, "", "")
			ctx := context.Background()

			stats, err := probe.GetDataStats(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(stats).ToNot(BeNil())
			Expect(stats.TotalBytes).To(BeNumerically(">", 0))
			Expect(stats.PercentUsed).To(BeNumerically(">=", 0))
			Expect(stats.PercentUsed).To(BeNumerically("<=", 100))
			Expect(stats.Path).To(Equal(tmpDir))
		})
	})

	Describe("GetWALStats", func() {
		It("returns nil for single volume clusters", func() {
			probe := NewProbeWithPaths("/tmp", "", "")
			ctx := context.Background()

			stats, err := probe.GetWALStats(ctx, false)
			Expect(err).ToNot(HaveOccurred())
			Expect(stats).To(BeNil())
		})

		It("returns data for separate WAL volume", func() {
			probe := NewProbeWithPaths("/tmp", tmpDir, "")
			ctx := context.Background()

			stats, err := probe.GetWALStats(ctx, true)
			Expect(err).ToNot(HaveOccurred())
			Expect(stats).ToNot(BeNil())
			Expect(stats.TotalBytes).To(BeNumerically(">", 0))
		})
	})
})
