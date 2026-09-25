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
	"context"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/event"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// pendingTargets remembers, per scope, the targets reported changed since the last sync.
type pendingTargets struct {
	mu sync.Mutex
	m  map[string]map[inventory.TargetKey]bool
}

func (p *pendingTargets) add(scope string, keys map[inventory.TargetKey]bool) {
	if len(keys) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]map[inventory.TargetKey]bool{}
	}
	if p.m[scope] == nil {
		p.m[scope] = map[inventory.TargetKey]bool{}
	}
	for k := range keys {
		p.m[scope][k] = true
	}
}

func (p *pendingTargets) take(scope string) map[inventory.TargetKey]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := p.m[scope]
	delete(p.m, scope)
	return keys
}

// NotifyChanged marks the given account/region pairs as changed in every scope that covers
// them and triggers a sync of those scopes. Pairs outside every scope are ignored.
// It is the sink of the EC2 event poller.
func (r *NetworkScopeReconciler) NotifyChanged(ctx context.Context, changed []inventory.TargetKey) error {
	set := map[inventory.TargetKey]bool{}
	for _, k := range changed {
		set[k] = true
	}
	scopes := &networkv1beta1.NetworkScopeList{}
	if err := r.List(ctx, scopes); err != nil {
		return err
	}
	for i := range scopes.Items {
		scope := &scopes.Items[i]
		matched := map[inventory.TargetKey]bool{}
		for _, t := range expandTargets(scope, nil) {
			if set[t.Key()] {
				matched[t.Key()] = true
			}
		}
		if len(matched) == 0 {
			continue
		}
		r.pending.add(scope.Name, matched)
		select {
		case r.changesChannel() <- event.GenericEvent{Object: scope}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *NetworkScopeReconciler) changesChannel() chan event.GenericEvent {
	r.changesOnce.Do(func() { r.changes = make(chan event.GenericEvent, 1024) })
	return r.changes
}
