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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("WAL Health Checker", func() {
	var tmpDir string

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "wal-health-test")
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		_ = os.RemoveAll(tmpDir)
	})

	Describe("CountPendingArchive", func() {
		It("counts .ready files correctly", func() {
			readyFiles := []string{
				"000000010000000000000001.ready",
				"000000010000000000000002.ready",
				"000000010000000000000003.ready",
				"000000010000000000000004.ready",
				"000000010000000000000005.ready",
			}

			for _, fileName := range readyFiles {
				filePath := filepath.Join(tmpDir, fileName)
				err := os.WriteFile(filePath, []byte{}, 0o600)
				Expect(err).ToNot(HaveOccurred())
			}

			checker := NewWALHealthCheckerWithPath(tmpDir)
			count, err := checker.CountPendingArchive()
			Expect(err).ToNot(HaveOccurred())
			Expect(count).To(Equal(5))
		})

		It("returns zero for empty directory", func() {
			checker := NewWALHealthCheckerWithPath(tmpDir)
			count, err := checker.CountPendingArchive()
			Expect(err).ToNot(HaveOccurred())
			Expect(count).To(Equal(0))
		})

		It("returns zero for non-existent directory", func() {
			checker := NewWALHealthCheckerWithPath("/nonexistent/path/archive_status")
			count, err := checker.CountPendingArchive()
			Expect(err).ToNot(HaveOccurred())
			Expect(count).To(Equal(0))
		})

		It("ignores non-.ready files", func() {
			files := []string{
				"000000010000000000000001.ready",
				"000000010000000000000002.done",
				"000000010000000000000003.ready",
				"some_other_file.txt",
			}

			for _, fileName := range files {
				filePath := filepath.Join(tmpDir, fileName)
				err := os.WriteFile(filePath, []byte{}, 0o600)
				Expect(err).ToNot(HaveOccurred())
			}

			checker := NewWALHealthCheckerWithPath(tmpDir)
			count, err := checker.CountPendingArchive()
			Expect(err).ToNot(HaveOccurred())
			Expect(count).To(Equal(2))
		})
	})
})
