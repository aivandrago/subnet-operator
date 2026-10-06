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

package utils

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnableKustomizeWebhooks turns the webhooks on in a copy of config/ at configDir, the way the
// comments in config/default/kustomization.yaml tell a user to: it uncomments the [WEBHOOK] and
// [CERTMANAGER] sections there and the conversion patches in config/crd. The render test and
// the e2e run with cert-manager both go through it, so what is rendered and checked on paper
// is what is deployed and checked in a cluster.
//
// Left commented: [PROMETHEUS], [NETWORK POLICY] and [METRICS-WITH-CERTS], which are switches
// of their own. The replacements of the metrics certificate are part of [CERTMANAGER] and are
// uncommented with the rest: config/certmanager brings that Certificate in any case, and
// without them it would ask for the placeholder name SERVICE_NAME.SERVICE_NAMESPACE.svc.
func EnableKustomizeWebhooks(configDir string) error {
	err := uncommentLines(filepath.Join(configDir, "crd", "kustomization.yaml"), func(line string) bool {
		return strings.HasPrefix(line, "#- path: patches/webhook_in_") ||
			line == "#configurations:" || line == "#- kustomizeconfig.yaml"
	})
	if err != nil {
		return err
	}
	return uncommentLines(filepath.Join(configDir, "default", "kustomization.yaml"), webhookSections())
}

// webhookSections decides, line by line, what enabling the webhooks with cert-manager
// uncomments in config/default: the webhook and certmanager resources, the manager's webhook
// patch with its target, and every replacement.
func webhookSections() func(string) bool {
	inReplacements, inPatch := false, false
	return func(line string) bool {
		switch {
		case line == "#- ../webhook" || line == "#- ../certmanager":
			return true
		case line == "#- path: manager_webhook_patch.yaml":
			inPatch = true
			return true
		case inPatch && strings.HasPrefix(line, "#  "):
			return true
		case line == "#replacements:":
			inPatch, inReplacements = false, true
			return true
		case !inReplacements:
			inPatch = false
			return false
		}
		// The scaffold markers stay comments: kubebuilder adds the next CRD's block above them.
		return strings.HasPrefix(line, "# ") && !strings.HasPrefix(line, "# +kubebuilder")
	}
}

// uncommentLines removes the leading "#" of every line uncomment says to, and fails when it
// says so of none: the file no longer looks the way this code reads it.
func uncommentLines(path string, uncomment func(string) bool) error {
	// #nosec G304 -- the path is a file of a copy of this repository's config/, not user input.
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", path, err)
	}
	lines := strings.Split(string(raw), "\n")
	changed := 0
	for i, line := range lines {
		if uncomment(line) {
			lines[i] = strings.TrimPrefix(line, "#")
			changed++
		}
	}
	if changed == 0 {
		return fmt.Errorf("nothing to uncomment in %q", path)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return fmt.Errorf("failed to write file %q: %w", path, err)
	}
	return nil
}
