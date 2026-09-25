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

package controller

import (
	"k8s.io/apimachinery/pkg/runtime"
	kevents "k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/audit"
)

// Event reasons. They are part of what `kubectl describe` shows and what people write alerts
// against, so they change as rarely as a metric name does.
const (
	// EventImported is on a ResourceImport whose tags reached AWS.
	EventImported = "Imported"
	// EventImportDryRun is on a ResourceImport that only computed its tags.
	EventImportDryRun = "DryRun"
	// EventAllocated is on a SubnetClaim that reserved CIDRs.
	EventAllocated = "Allocated"
	// EventSubnetCreated is on a SubnetClaim whose subnet now exists in AWS.
	EventSubnetCreated = "SubnetCreated"
	// EventAutoImportRequested is on a NetworkScope whose policy wrote a ResourceImport.
	EventAutoImportRequested = "AutoImportRequested"
	// EventNoOwner is on a NetworkScope that found a resource no rule could attribute.
	EventNoOwner = "NoOwner"
	// EventTargetUnreachable is on a NetworkScope with an account/region it could not read.
	EventTargetUnreachable = "TargetUnreachable"
	// EventTargetThrottled is on a NetworkScope with an account/region the API rate-limited:
	// reachable, but backed off and stale until a later attempt gets through.
	EventTargetThrottled = "TargetThrottled"
	// EventNamespacesUnrestricted is on a NetworkScope whose namespaceSelector is empty, which
	// lets every namespace use it. Emitted once per change of the spec.
	EventNamespacesUnrestricted = "NamespacesUnrestricted"

	// A Warning Event that explains why something did not happen has no constant here: it
	// reuses the reason of the status condition it accompanies (ScopeNotFound, WritesDisabled,
	// NoSpace…), so an object's conditions and its Events answer a question with one word.
)

// Event actions say what the operator was doing to the object, in the events.k8s.io sense.
const (
	// ActionApplyTags is tagging an existing resource to take it under management.
	ActionApplyTags = "ApplyTags"
	// ActionReserveCIDR is reserving address space for a claim.
	ActionReserveCIDR = "ReserveCIDR"
	// ActionCreateSubnet is creating a subnet for a claim.
	ActionCreateSubnet = "CreateSubnet"
	// ActionAutoImport is deciding what to do with a resource nobody has tagged.
	ActionAutoImport = "AutoImport"
	// ActionDiscover is reading an account and region.
	ActionDiscover = "Discover"
	// ActionCheckNamespaces is deciding which namespaces may use a scope.
	ActionCheckNamespaces = "CheckNamespaces"
)

// eventf records an Event, tolerating a nil recorder so a reconciler built without one (a
// test, an operator running with no permission to write Events) needs no branch of its own.
func eventf(recorder kevents.EventRecorder, obj runtime.Object, eventType, reason, action, note string, args ...any) {
	if recorder == nil {
		return
	}
	recorder.Eventf(obj, nil, eventType, reason, action, note, args...)
}

// objectRef names the object an audit line came from, as Kind/namespace/name. It is what
// lets somebody go from a line in the SIEM back to the thing a human applied.
func objectRef(kind string, obj client.Object) string {
	if obj.GetNamespace() == "" {
		return kind + "/" + obj.GetName()
	}
	return kind + "/" + obj.GetNamespace() + "/" + obj.GetName()
}

// createdBy is the created_by of an audit line about obj. The annotation is only worth
// something when the operator's own admission webhooks wrote it and guard it; without them it
// is whatever the object's author typed, and the line says so instead of repeating it.
func createdBy(obj client.Object, webhooksEnabled bool) string {
	if !webhooksEnabled {
		return audit.CreatedByUnknown
	}
	if v := obj.GetAnnotations()[networkv1.AnnotationCreatedBy]; v != "" {
		return v
	}
	return audit.CreatedByUnknown
}
