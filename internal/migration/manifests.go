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

package migration

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
)

// Manifests rewrites a stream of YAML documents for the new group, the way
// `manager migrate-manifests` does for a GitOps repository: aws.hypersurgery/v1alpha1
// NetworkScope, SubnetClaim, ResourceImport and SheetExport documents are converted with the
// same functions the migration controller uses; VPC and Subnet documents are left out, because
// the operator writes those itself; every other document is copied unchanged, byte for byte.
//
// A converted document loses its comments and its status (apply ignores status anyway). What a
// human should know about a conversion goes to notes, one line per note, naming the document.
func Manifests(in io.Reader, out, notes io.Writer) error {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(in))
	first := true
	for index := 0; ; index++ {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("document %d: %w", index+1, err)
		}
		converted, keep, docNotes, err := convertDocument(doc)
		if err != nil {
			return fmt.Errorf("document %d: %w", index+1, err)
		}
		for _, n := range docNotes {
			if _, err := fmt.Fprintf(notes, "document %d: %s\n", index+1, n); err != nil {
				return err
			}
		}
		if !keep {
			continue
		}
		if !first {
			if _, err := io.WriteString(out, "---\n"); err != nil {
				return err
			}
		}
		first = false
		if _, err := out.Write(converted); err != nil {
			return err
		}
	}
}

// convertDocument converts one document. keep is false for a document to leave out.
func convertDocument(doc []byte) (out []byte, keep bool, notes Notes, err error) {
	var tm metav1.TypeMeta
	if len(bytes.TrimSpace(doc)) == 0 {
		return doc, false, nil, nil
	}
	if err := yaml.Unmarshal(doc, &tm); err != nil {
		return nil, false, nil, err
	}
	if tm.APIVersion != OldAPIVersion {
		// Not ours: copied as it is. It may still name the old group — an RBAC rule, a label
		// selector — which only a human can tell apart from a coincidence.
		if bytes.Contains(doc, []byte("aws.hypersurgery")) {
			notes = Notes{"still mentions aws.hypersurgery (an RBAC rule, a label or a selector?); " +
				"change it to network.hypersurgery.dev by hand, and vpcs to networks"}
		}
		return doc, true, notes, nil
	}
	name := func(meta metav1.ObjectMeta) string {
		if meta.Namespace != "" {
			return tm.Kind + " " + meta.Namespace + "/" + meta.Name
		}
		return tm.Kind + " " + meta.Name
	}

	var obj any
	switch tm.Kind {
	case kindNetworkScope:
		old := &awsv1alpha1.NetworkScope{}
		if err := yaml.UnmarshalStrict(doc, old); err != nil {
			return nil, false, nil, err
		}
		scope, n := NetworkScope(old, ForManifest)
		scope.Status = networkv1beta1.NetworkScopeStatus{}
		obj, notes = scope, prefixed(name(old.ObjectMeta), n)
	case kindSubnetClaim:
		old := &awsv1alpha1.SubnetClaim{}
		if err := yaml.UnmarshalStrict(doc, old); err != nil {
			return nil, false, nil, err
		}
		claim, n := SubnetClaim(old, ForManifest)
		claim.Status = networkv1beta1.SubnetClaimStatus{}
		if len(old.Status.Allocations) > 0 {
			n.add("status.allocations are not written to a manifest; the migration controller copies them in the cluster")
		}
		obj, notes = claim, prefixed(name(old.ObjectMeta), n)
	case kindResourceImport:
		old := &awsv1alpha1.ResourceImport{}
		if err := yaml.UnmarshalStrict(doc, old); err != nil {
			return nil, false, nil, err
		}
		imp, n := ResourceImport(old, ForManifest)
		imp.Status = networkv1beta1.ResourceImportStatus{}
		obj, notes = imp, prefixed(name(old.ObjectMeta), n)
	case kindSheetExport:
		old := &awsv1alpha1.SheetExport{}
		if err := yaml.UnmarshalStrict(doc, old); err != nil {
			return nil, false, nil, err
		}
		exp, n := SheetExport(old, ForManifest)
		exp.Status = networkv1beta1.SheetExportStatus{}
		obj, notes = exp, prefixed(name(old.ObjectMeta), n)
	case "VPC", "Subnet":
		var meta struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		_ = yaml.Unmarshal(doc, &meta)
		return nil, false, Notes{fmt.Sprintf("%s: left out; the operator discovers it again as a network.hypersurgery.dev object",
			name(meta.Metadata))}, nil
	default:
		return nil, false, nil, fmt.Errorf("%s %s is not a kind of that group", tm.APIVersion, tm.Kind)
	}
	out, err = yaml.Marshal(obj)
	if err != nil {
		return nil, false, nil, err
	}
	return out, true, notes, nil
}

func prefixed(name string, notes Notes) Notes {
	out := make(Notes, 0, len(notes))
	for _, n := range notes {
		out = append(out, name+": "+strings.TrimSpace(n))
	}
	return out
}
