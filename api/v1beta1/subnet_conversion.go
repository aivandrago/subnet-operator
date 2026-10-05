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
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// ConvertTo converts this Subnet to the hub version, v1.
func (src *Subnet) ConvertTo(dstRaw conversion.Hub) error {
	dst, ok := dstRaw.(*networkv1.Subnet)
	if !ok {
		return unexpectedHub(dstRaw)
	}
	return convertObject(&src.ObjectMeta, &dst.ObjectMeta, [2]any{&src.Spec, &dst.Spec}, [2]any{&src.Status, &dst.Status})
}

// ConvertFrom converts the hub version, v1, to this Subnet.
func (dst *Subnet) ConvertFrom(srcRaw conversion.Hub) error {
	src, ok := srcRaw.(*networkv1.Subnet)
	if !ok {
		return unexpectedHub(srcRaw)
	}
	return convertObject(&src.ObjectMeta, &dst.ObjectMeta, [2]any{&src.Spec, &dst.Spec}, [2]any{&src.Status, &dst.Status})
}
