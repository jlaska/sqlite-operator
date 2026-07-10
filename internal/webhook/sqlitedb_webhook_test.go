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

package webhook_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasev1 "github.com/jlaska/sqlite-operator/api/v1"
	"github.com/jlaska/sqlite-operator/internal/webhook"
)

// newValidDB returns a minimal valid SQLiteDB for use in validation tests.
func newValidDB() *databasev1.SQLiteDB {
	return &databasev1.SQLiteDB{
		ObjectMeta: metav1.ObjectMeta{Name: "test-db", Namespace: "default"},
		Spec: databasev1.SQLiteDBSpec{
			TargetDeployment: "my-app",
			DatabaseName:     "app.db",
			DatabasePath:     "/data",
		},
	}
}

var _ = Describe("SQLiteDBValidator", func() {
	var validator *webhook.SQLiteDBValidator

	BeforeEach(func() {
		validator = &webhook.SQLiteDBValidator{}
	})

	ctx := context.Background()

	Describe("ValidateDelete", func() {
		It("always permits deletion", func() {
			db := newValidDB()
			warnings, err := validator.ValidateDelete(ctx, db)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})
	})

	Describe("ValidateCreate and ValidateUpdate", func() {
		type testCase struct {
			description string
			mutate      func(*databasev1.SQLiteDB)
			expectError bool
			errContains string
		}

		cases := []testCase{
			{
				description: "valid: all required fields set",
				mutate:      func(_ *databasev1.SQLiteDB) {},
				expectError: false,
			},
			{
				description: "invalid: missing targetDeployment",
				mutate:      func(db *databasev1.SQLiteDB) { db.Spec.TargetDeployment = "" },
				expectError: true,
				errContains: "targetDeployment",
			},
			{
				description: "invalid: missing databaseName",
				mutate:      func(db *databasev1.SQLiteDB) { db.Spec.DatabaseName = "" },
				expectError: true,
				errContains: "databaseName",
			},
			{
				description: "invalid: missing databasePath",
				mutate:      func(db *databasev1.SQLiteDB) { db.Spec.DatabasePath = "" },
				expectError: true,
				errContains: "databasePath",
			},
			{
				description: "invalid: backup enabled but S3 not configured",
				mutate: func(db *databasev1.SQLiteDB) {
					db.Spec.Backup.Enabled = true
					// Destination.S3 is nil by default
				},
				expectError: true,
				errContains: "s3",
			},
			{
				description: "invalid: backup enabled, S3 set, but bucket empty",
				mutate: func(db *databasev1.SQLiteDB) {
					db.Spec.Backup = databasev1.BackupSpec{
						Enabled: true,
						Destination: databasev1.BackupDestination{
							S3: &databasev1.S3Destination{Bucket: "", SecretRef: "my-secret"},
						},
					}
				},
				expectError: true,
				errContains: "bucket",
			},
			{
				description: "invalid: backup enabled, S3 set, but secretRef empty",
				mutate: func(db *databasev1.SQLiteDB) {
					db.Spec.Backup = databasev1.BackupSpec{
						Enabled: true,
						Destination: databasev1.BackupDestination{
							S3: &databasev1.S3Destination{Bucket: "my-bucket", SecretRef: ""},
						},
					}
				},
				expectError: true,
				errContains: "secretRef",
			},
			{
				description: "valid: backup enabled with fully-set S3 config",
				mutate: func(db *databasev1.SQLiteDB) {
					db.Spec.Backup = databasev1.BackupSpec{
						Enabled: true,
						Destination: databasev1.BackupDestination{
							S3: &databasev1.S3Destination{Bucket: "my-bucket", SecretRef: "my-secret"},
						},
					}
				},
				expectError: false,
			},
			{
				description: "valid: backup disabled, S3 not set",
				mutate: func(db *databasev1.SQLiteDB) {
					db.Spec.Backup = databasev1.BackupSpec{Enabled: false}
				},
				expectError: false,
			},
			{
				description: "invalid: multiple missing fields produces aggregate error",
				mutate: func(db *databasev1.SQLiteDB) {
					db.Spec.TargetDeployment = ""
					db.Spec.DatabaseName = ""
				},
				expectError: true,
				errContains: "targetDeployment",
			},
		}

		for _, tc := range cases {
			tc := tc // capture loop variable
			It(tc.description, func() {
				db := newValidDB()
				tc.mutate(db)

				// ValidateCreate
				warnings, err := validator.ValidateCreate(ctx, db)
				if tc.expectError {
					Expect(err).To(HaveOccurred(), "ValidateCreate should have returned an error")
					Expect(err.Error()).To(ContainSubstring(tc.errContains),
						"error should mention %q", tc.errContains)
				} else {
					Expect(err).NotTo(HaveOccurred(), "ValidateCreate should not have returned an error")
				}
				Expect(warnings).To(BeEmpty())

				// ValidateUpdate uses the same validation logic — spot-check it produces
				// the same outcome so both wrappers are exercised.
				oldDB := newValidDB() // old state doesn't affect validation
				_, updateErr := validator.ValidateUpdate(ctx, oldDB, db)
				if tc.expectError {
					Expect(updateErr).To(HaveOccurred())
				} else {
					Expect(updateErr).NotTo(HaveOccurred())
				}
			})
		}
	})
})
