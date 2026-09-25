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
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
)

// Legacy answers the new group's controllers while the old group is still served: whether an
// object's old counterpart still waits to be migrated into it, and which CIDRs unmigrated old
// claims hold. It reads through the manager's cache, which watches the old kinds for the
// migration controller anyway. It goes away with the old group in 0.9.
type Legacy struct {
	Client client.Reader
}

// Pending reports whether obj's aws.hypersurgery/v1alpha1 counterpart — same kind, name and
// namespace — exists and is not migrated yet. Its controller must then leave obj alone: the
// counterpart's status is about to be copied into it, and acting first would re-reserve CIDRs,
// re-apply tags, or count known unmanaged resources as new.
func (l *Legacy) Pending(ctx context.Context, obj client.Object) (bool, error) {
	var old client.Object
	switch obj.(type) {
	case *networkv1beta1.NetworkScope:
		old = &awsv1alpha1.NetworkScope{}
	case *networkv1beta1.SubnetClaim:
		old = &awsv1alpha1.SubnetClaim{}
	case *networkv1beta1.ResourceImport:
		old = &awsv1alpha1.ResourceImport{}
	case *networkv1beta1.SheetExport:
		old = &awsv1alpha1.SheetExport{}
	default:
		return false, fmt.Errorf("no aws.hypersurgery/v1alpha1 kind for %T", obj)
	}
	if err := l.Client.Get(ctx, client.ObjectKeyFromObject(obj), old); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	if !old.GetDeletionTimestamp().IsZero() {
		return false, nil
	}
	_, done := old.GetAnnotations()[networkv1beta1.AnnotationMigratedTo]
	return !done, nil
}

// Reservations returns the CIDRs that unmigrated old-group claims hold in a network. Once a
// claim is migrated its reservations are in the new claim and counted from there.
func (l *Legacy) Reservations(ctx context.Context, networkID string) ([]string, error) {
	claims := &awsv1alpha1.SubnetClaimList{}
	if err := l.Client.List(ctx, claims); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	var used []string
	for i := range claims.Items {
		c := &claims.Items[i]
		if _, done := c.Annotations[networkv1beta1.AnnotationMigratedTo]; done || c.Spec.VPCID != networkID {
			continue
		}
		for _, a := range c.Status.Allocations {
			used = append(used, a.CIDRBlock)
		}
	}
	return used, nil
}

// pendingObjects counts the old-group objects that are not migrated yet. 0.9 refuses to start
// while any exist, so this is the number to watch before upgrading to it.
var pendingObjects = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "hs_migration_pending_objects",
	Help: "aws.hypersurgery/v1alpha1 objects not migrated to network.hypersurgery.dev yet, by kind. " +
		"0.9 does not start while any exist.",
}, []string{"kind"})

func init() {
	ctrlmetrics.Registry.MustRegister(pendingObjects)
}

// countInterval is how often the pending objects are counted.
const countInterval = 30 * time.Second

// PendingCounter counts the unmigrated old-group objects for hs_migration_pending_objects. It
// runs on every replica: it only reads the cache, and a standby's metrics should say the same.
type PendingCounter struct {
	Client client.Reader
}

// NeedLeaderElection lets every replica count.
func (c *PendingCounter) NeedLeaderElection() bool { return false }

// Start counts until the manager stops.
func (c *PendingCounter) Start(ctx context.Context) error {
	ticker := time.NewTicker(countInterval)
	defer ticker.Stop()
	for {
		c.count(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (c *PendingCounter) count(ctx context.Context) {
	lists := map[string]client.ObjectList{
		kindNetworkScope:   &awsv1alpha1.NetworkScopeList{},
		kindSubnetClaim:    &awsv1alpha1.SubnetClaimList{},
		kindResourceImport: &awsv1alpha1.ResourceImportList{},
		kindSheetExport:    &awsv1alpha1.SheetExportList{},
	}
	for kind, list := range lists {
		if err := c.Client.List(ctx, list); err != nil {
			logf.FromContext(ctx).Error(err, "Could not count unmigrated objects", "kind", kind)
			continue
		}
		pending := 0
		_ = meta.EachListItem(list, func(o runtime.Object) error {
			obj, ok := o.(client.Object)
			if !ok {
				return nil
			}
			if _, done := obj.GetAnnotations()[networkv1beta1.AnnotationMigratedTo]; !done {
				pending++
			}
			return nil
		})
		pendingObjects.WithLabelValues(kind).Set(float64(pending))
	}
}
