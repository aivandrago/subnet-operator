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

// Package sheets writes the subnet inventory into a Google Sheet, for people who prefer a
// table. The sheet is an output only: the operator rewrites it and never reads it back.
package sheets

import (
	"cmp"
	"context"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
)

// Table is what ends up in the sheet: one header row and one row per subnet.
type Table struct {
	Header []string
	Rows   [][]string
}

// Syncer writes a table into one tab of a spreadsheet.
type Syncer interface {
	Sync(ctx context.Context, spreadsheetID, sheetName string, t Table) error
}

var baseColumns = []string{
	"Account", "Region", "VPC", "VPC name", "Subnet", "Name", "CIDR", "AZ",
	"Public", "Tier", "Owner", "Environment", "Total IPs", "Free IPs", "Used %",
	"Missing tags", "Route table", "Data as of (UTC)",
}

// BuildTable renders the subnets, sorted by account, region, VPC and CIDR. vpcNames and
// syncedAt are looked up by VPC ID and by "account/region"; missing entries are left empty.
func BuildTable(subnets []awsv1alpha1.Subnet, vpcNames map[string]string,
	syncedAt map[string]*metav1.Time, extraTagColumns []string) Table {
	t := Table{Header: slices.Concat(baseColumns, extraTagColumns)}

	sorted := slices.Clone(subnets)
	slices.SortFunc(sorted, func(a, b awsv1alpha1.Subnet) int {
		if c := cmp.Or(
			cmp.Compare(a.Spec.Account, b.Spec.Account),
			cmp.Compare(a.Spec.Region, b.Spec.Region),
			cmp.Compare(a.Spec.VPCID, b.Spec.VPCID),
		); c != 0 {
			return c
		}
		if c := compareCIDR(a.Status.CIDRBlock, b.Status.CIDRBlock); c != 0 {
			return c
		}
		return cmp.Compare(a.Spec.SubnetID, b.Spec.SubnetID)
	})

	for _, s := range sorted {
		var asOf string
		if ts := syncedAt[s.Spec.Account+"/"+s.Spec.Region]; ts != nil {
			asOf = ts.UTC().Format("2006-01-02 15:04")
		}
		row := make([]string, 0, len(baseColumns)+len(extraTagColumns))
		row = append(row,
			s.Spec.Account,
			s.Spec.Region,
			s.Spec.VPCID,
			vpcNames[s.Spec.VPCID],
			s.Spec.SubnetID,
			s.Status.Name,
			s.Status.CIDRBlock,
			s.Status.AvailabilityZone,
			strconv.FormatBool(s.Status.Public),
			s.Status.Tier,
			s.Status.Owner,
			s.Status.Env,
			strconv.FormatInt(s.Status.TotalIPs, 10),
			strconv.FormatInt(s.Status.AvailableIPs, 10),
			strconv.Itoa(int(s.Status.UtilizationPercent)),
			strings.Join(s.Status.MissingTags, ", "),
			s.Status.RouteTableID,
			asOf,
		)
		for _, key := range extraTagColumns {
			row = append(row, s.Status.Tags[key])
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

// compareCIDR orders subnets by address, so 10.0.9.0/24 comes before 10.0.10.0/24.
// Unparsable values sort last, by string.
func compareCIDR(a, b string) int {
	pa, errA := netip.ParsePrefix(a)
	pb, errB := netip.ParsePrefix(b)
	switch {
	case errA != nil && errB != nil:
		return cmp.Compare(a, b)
	case errA != nil:
		return 1
	case errB != nil:
		return -1
	}
	return cmp.Or(pa.Addr().Compare(pb.Addr()), cmp.Compare(pa.Bits(), pb.Bits()))
}
