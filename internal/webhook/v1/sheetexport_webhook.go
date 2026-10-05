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
	ctrl "sigs.k8s.io/controller-runtime"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// SetupSheetExportWebhookWithManager registers the conversion of SheetExport between v1beta1
// and v1. It has no admission webhook: its controller reports what is wrong with an export.
func SetupSheetExportWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &networkv1.SheetExport{}).Complete()
}
