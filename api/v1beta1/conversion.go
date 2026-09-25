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
	"bytes"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

// v1beta1 and v1 have the same fields (ADR 0002 §10: v1 is v1beta1 minus what beta deprecated,
// which is nothing), so a spec or a status converts by going through its JSON form: the JSON
// of one version is the JSON of the other. The API server keeps the conversion webhook for it
// all the same, so the first real difference only has to change these functions.
//
// Decoding refuses a field the target does not have. Once v1 gains a field v1beta1 lacks, a
// v1 object carrying it fails to convert down, loudly, instead of losing the field in a
// round trip; that is the moment to write the conversion by hand (and to keep the field in an
// annotation, the usual way). The round-trip tests fill every field and catch the rest.
func convertFields(src, dst any) error {
	raw, err := json.Marshal(src)
	if err != nil {
		return fmt.Errorf("converting %T: %w", src, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("converting %T to %T: %w", src, dst, err)
	}
	return nil
}

// convertObject copies the metadata and converts the spec and the status, each a source and a
// target.
func convertObject(srcMeta, dstMeta *metav1.ObjectMeta, spec, status [2]any) error {
	srcMeta.DeepCopyInto(dstMeta)
	if err := convertFields(spec[0], spec[1]); err != nil {
		return err
	}
	return convertFields(status[0], status[1])
}

func unexpectedHub(hub conversion.Hub) error {
	return fmt.Errorf("cannot convert network.hypersurgery.dev/v1beta1 from or to %T", hub)
}
