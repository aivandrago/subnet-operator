/*
Copyright 2026.

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

package v1beta1

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCertificateAvailable covers the check that decides whether the manager registers the
// webhooks at all. An install without certificates has to leave them off and keep running:
// the inventory is the operator's job, and admission is an addition to it.
func TestCertificateAvailable(t *testing.T) {
	dir := t.TempDir()

	if CertificateAvailable(dir, "tls.crt") {
		t.Error("an empty directory should not count as a certificate")
	}

	if err := os.WriteFile(filepath.Join(dir, "empty.crt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if CertificateAvailable(dir, "empty.crt") {
		t.Error("a zero-length file should not count as a certificate")
	}

	if err := os.Mkdir(filepath.Join(dir, "directory.crt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if CertificateAvailable(dir, "directory.crt") {
		t.Error("a directory should not count as a certificate")
	}

	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("-----BEGIN CERTIFICATE-----"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !CertificateAvailable(dir, "tls.crt") {
		t.Error("a certificate that is there should be found")
	}
}
