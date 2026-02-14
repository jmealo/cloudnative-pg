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
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDisk(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Disk Suite")
}

var _ = Describe("Disk probe", func() {
	Describe("Probe", func() {
		It("should return valid disk status for current directory", func() {
			status, err := Probe(".")
			Expect(err).ToNot(HaveOccurred())
			Expect(status).ToNot(BeNil())
			Expect(status.TotalBytes).To(BeNumerically(">", 0))
			Expect(status.UsedBytes).To(BeNumerically(">", 0))
			Expect(status.PercentUsed).To(BeNumerically(">=", 0))
			Expect(status.PercentUsed).To(BeNumerically("<=", 100))
		})

		It("should return valid disk status for temp directory", func() {
			tmpDir, err := os.MkdirTemp("", "disk-probe-test")
			Expect(err).ToNot(HaveOccurred())
			defer func() { _ = os.RemoveAll(tmpDir) }()

			status, err := Probe(tmpDir)
			Expect(err).ToNot(HaveOccurred())
			Expect(status).ToNot(BeNil())
			Expect(status.TotalBytes).To(BeNumerically(">", 0))
			// Available should be <= total
			Expect(status.AvailableBytes).To(BeNumerically("<=", status.TotalBytes))
		})

		It("should return error for non-existent path", func() {
			status, err := Probe("/nonexistent/path/that/does/not/exist")
			Expect(err).To(HaveOccurred())
			Expect(status).To(BeNil())
		})
	})
})
