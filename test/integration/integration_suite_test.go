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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/jlaska/sqlite-operator/test/utils"
)

const (
	operatorNamespace = "sqlite-operator-system"
	testNamespace     = "sqlite-integration"

	// MinIO access credentials (used in Secret and Litestream config).
	minioUser   = "minioadmin"
	minioPass   = "minioadmin"
	minioBucket = "sqlite-backups"
	// minioEndpoint is the in-cluster address Litestream uses.
	minioEndpoint = "minio." + testNamespace + ".svc.cluster.local:9000"
)

func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting sqlite-operator integration test suite\n")
	RunSpecs(t, "Integration Suite")
}

var _ = BeforeSuite(func() {
	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(5 * time.Second)

	By("creating test namespace")
	// --dry-run + apply pattern is idempotent.
	kubectl("create", "namespace", testNamespace, "--dry-run=client", "-o", "yaml")
	runIgnoreError("kubectl", "create", "namespace", testNamespace)

	By("deploying MinIO in test namespace")
	applyLiteral(minioManifest())

	By("waiting for MinIO pod to be Running")
	kubectl("wait", "-n", testNamespace, "deployment/minio",
		"--for=condition=Available", "--timeout=3m")

	By("creating MinIO bucket via mc Job")
	applyLiteral(createBucketJobManifest())
	kubectl("wait", "-n", testNamespace, "job/minio-create-bucket",
		"--for=condition=Complete", "--timeout=2m")

	By("creating MinIO credentials Secret")
	runIgnoreError("kubectl", "create", "secret", "generic", "minio-creds",
		"-n", testNamespace,
		"--from-literal=access-key-id="+minioUser,
		"--from-literal=secret-access-key="+minioPass,
	)
})

var _ = AfterSuite(func() {
	if os.Getenv("INTEGRATION_KEEP_NAMESPACE") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "INTEGRATION_KEEP_NAMESPACE=true — skipping namespace cleanup\n")
		return
	}
	By("removing test namespace")
	runIgnoreError("kubectl", "delete", "namespace", testNamespace,
		"--ignore-not-found", "--timeout=3m")
})

// ── helpers ────────────────────────────────────────────────────────────────

// kubectl runs a kubectl command and fails the test immediately on error.
// Do NOT call this inside Eventually — use kubectlQ instead.
func kubectl(args ...string) string {
	out, err := kubectlQ(args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "kubectl %v failed:\n%s", args, out)
	return out
}

// kubectlQ runs kubectl and returns (output, error) without failing the test.
// Use inside Eventually so errors cause a retry rather than aborting the spec.
func kubectlQ(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	return utils.Run(cmd)
}

// runIgnoreError runs a command and swallows any error (for idempotent ops).
func runIgnoreError(name string, args ...string) {
	cmd := exec.Command(name, args...)
	_, _ = utils.Run(cmd)
}

// applyLiteral writes a YAML string to a temp file and applies it.
func applyLiteral(yaml string) {
	f, err := os.CreateTemp("", "sqlite-integration-*.yaml")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.WriteString(yaml)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	_ = f.Close()
	kubectl("apply", "-f", f.Name())
}

// ── static manifests ───────────────────────────────────────────────────────

func minioManifest() string {
	return fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: minio
  template:
    metadata:
      labels:
        app: minio
    spec:
      containers:
        - name: minio
          image: quay.io/minio/minio:latest
          args: ["server", "/data", "--console-address", ":9001"]
          env:
            - name: MINIO_ROOT_USER
              value: "%s"
            - name: MINIO_ROOT_PASSWORD
              value: "%s"
          ports:
            - containerPort: 9000
            - containerPort: 9001
---
apiVersion: v1
kind: Service
metadata:
  name: minio
  namespace: %s
spec:
  selector:
    app: minio
  ports:
    - name: api
      port: 9000
      targetPort: 9000
`, testNamespace, minioUser, minioPass, testNamespace)
}

func createBucketJobManifest() string {
	return fmt.Sprintf(`
apiVersion: batch/v1
kind: Job
metadata:
  name: minio-create-bucket
  namespace: %s
spec:
  backoffLimit: 3
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: mc
          image: quay.io/minio/mc:latest
          command:
            - /bin/sh
            - -c
            - |
              mc alias set local http://minio:9000 %s %s
              mc mb --ignore-existing local/%s
              echo "Bucket %s ready"
`, testNamespace, minioUser, minioPass, minioBucket, minioBucket)
}
