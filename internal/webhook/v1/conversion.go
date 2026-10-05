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

package v1

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/conversion"
)

// ConvertPath is where the API server sends conversion reviews.
const ConvertPath = "/convert"

// SetupConversionWebhookWithManager serves the conversion between v1beta1 and v1 at
// ConvertPath: controller-runtime's conversion webhook, which converts through the Go types
// (Hub and Convertible), behind ExactConversion. Call it before the other Setup functions, so
// the builders find the path taken and do not register a second handler.
func SetupConversionWebhookWithManager(mgr ctrl.Manager) {
	typed := conversion.NewWebhookHandler(mgr.GetScheme(), mgr.GetConverterRegistry())
	mgr.GetWebhookServer().Register(ConvertPath, ExactConversion(mgr.GetScheme(), typed))
}

// ExactConversion keeps an object as it was written wherever the conversion does not change
// what it means.
//
// Converting through Go types writes every value the way Go writes it back: a resyncInterval
// of "5m" comes back as "5m0s", and an explicit `dryRun: false` disappears. That changes
// nothing about the object, but a client reading it at the other version, a GitOps tool above
// all, sees a difference from what it applied and reports drift. So for each object the typed
// conversion (next) stays the judge of what the converted object is, and when the original,
// with only its apiVersion changed, decodes to exactly that, the original is returned instead.
// The day the versions differ in a field, the typed result is returned for the objects that
// use it.
func ExactConversion(scheme *runtime.Scheme, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)

		out := rec.Body.Bytes()
		if rec.Code == http.StatusOK {
			if exact, ok := preferOriginals(scheme, body, out); ok {
				out = exact
			}
		}
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, _ = w.Write(out)
	})
}

// preferOriginals replaces each converted object of a successful review by its original with
// the new apiVersion, where the two decode to the same object. ok is false when the review
// cannot be read, and the typed response then goes out unchanged.
func preferOriginals(scheme *runtime.Scheme, request, response []byte) ([]byte, bool) {
	var in, out apiextensionsv1.ConversionReview
	if json.Unmarshal(request, &in) != nil || json.Unmarshal(response, &out) != nil ||
		in.Request == nil || out.Response == nil || out.Response.Result.Status != metav1.StatusSuccess ||
		len(in.Request.Objects) != len(out.Response.ConvertedObjects) {
		return nil, false
	}
	gv, err := schema.ParseGroupVersion(in.Request.DesiredAPIVersion)
	if err != nil {
		return nil, false
	}
	for i, original := range in.Request.Objects {
		if exact, ok := sameMeaning(scheme, gv, original.Raw, out.Response.ConvertedObjects[i].Raw); ok {
			out.Response.ConvertedObjects[i] = runtime.RawExtension{Raw: exact}
		}
	}
	raw, err := json.Marshal(&out)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// sameMeaning returns the original object moved to gv, if it decodes to the same Go object as
// the converted one.
func sameMeaning(scheme *runtime.Scheme, gv schema.GroupVersion, original, converted []byte) ([]byte, bool) {
	var fields map[string]any
	dec := json.NewDecoder(bytes.NewReader(original))
	dec.UseNumber() // numbers stay as written
	if dec.Decode(&fields) != nil {
		return nil, false
	}
	kind, _ := fields["kind"].(string)
	fields["apiVersion"] = gv.String()
	moved, err := json.Marshal(fields)
	if err != nil {
		return nil, false
	}

	fromOriginal, err1 := decodeStrict(scheme, gv.WithKind(kind), moved)
	fromConverted, err2 := decodeStrict(scheme, gv.WithKind(kind), converted)
	if err1 != nil || err2 != nil || !apiequality.Semantic.DeepEqual(fromOriginal, fromConverted) {
		return nil, false
	}
	return moved, true
}

// decodeStrict decodes into the scheme's type for gvk and fails on a field the type lacks.
func decodeStrict(scheme *runtime.Scheme, gvk schema.GroupVersionKind, raw []byte) (runtime.Object, error) {
	obj, err := scheme.New(gvk)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(obj); err != nil {
		return nil, err
	}
	return obj, nil
}
