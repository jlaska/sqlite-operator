/*
Copyright 2025.

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

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
)

// runCmd executes a command, printing output to GinkgoWriter and returning
// combined output. Unlike kubectl(), it does not fail the test on error.
func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)

	// Run from the project root so relative paths work.
	if root := projectRoot(); root != "" {
		cmd.Dir = root
	}
	cmd.Env = append(os.Environ(), "GO111MODULE=on")

	_, _ = fmt.Fprintf(GinkgoWriter, "$ %s %s\n", name, strings.Join(args, " "))
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", string(out))
	}
	return string(out), err
}

// projectRoot walks up from the current working directory to find the module root
// (directory containing go.mod). Returns empty string if not found.
func projectRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		parent := dir[:strings.LastIndex(dir, "/")]
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
