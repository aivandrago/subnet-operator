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

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
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
	"Provider", "Account", "Region", "Network", "Network name", "Subnet", "Name", "CIDR", "Zone",
	"Public", "Tier", "Owner", "Environment", "Total IPs", "Free IPs", "Used %",
	"Missing tags", "Route table", "Data as of (UTC)",
}

// BuildTable renders the subnets, sorted by provider, account, region, network and CIDR.
// networkNames and syncedAt are looked up by network ID and by "account/region"; missing entries
// are left empty. Counts a provider could not report are left empty too, rather than written as
// zero.
func BuildTable(subnets []networkv1beta1.Subnet, networkNames map[string]string,
	syncedAt map[string]*metav1.Time, extraTagColumns []string) Table {
	t := Table{Header: slices.Concat(baseColumns, extraTagColumns)}

	sorted := slices.Clone(subnets)
	slices.SortFunc(sorted, func(a, b networkv1beta1.Subnet) int {
		if c := cmp.Or(
			cmp.Compare(a.Spec.Provider, b.Spec.Provider),
			cmp.Compare(a.Spec.Account, b.Spec.Account),
			cmp.Compare(a.Spec.Region, b.Spec.Region),
			cmp.Compare(a.Spec.NetworkID, b.Spec.NetworkID),
		); c != 0 {
			return c
		}
		if c := compareCIDR(a.Status.CIDRBlock, b.Status.CIDRBlock); c != 0 {
			return c
		}
		return cmp.Compare(a.Spec.ID, b.Spec.ID)
	})

	for _, s := range sorted {
		var asOf string
		if ts := syncedAt[s.Spec.Account+"/"+s.Spec.Region]; ts != nil {
			asOf = ts.UTC().Format("2006-01-02 15:04")
		}
		row := make([]string, 0, len(baseColumns)+len(extraTagColumns))
		aws := s.Status.AWSStatus()
		row = append(row,
			string(s.Spec.Provider),
			s.Spec.Account,
			s.Spec.Region,
			s.Spec.NetworkID,
			networkNames[s.Spec.NetworkID],
			s.Spec.ID,
			s.Status.Name,
			s.Status.CIDRBlock,
			s.Status.Zone,
			strconv.FormatBool(aws.Public),
			s.Status.Tier,
			s.Status.Owner,
			s.Status.Env,
			formatCount(s.Status.TotalIPs),
			formatCount(s.Status.AvailableIPs),
			formatPercent(s.Status.UtilizationPercent),
			strings.Join(s.Status.MissingTags, ", "),
			aws.RouteTableID,
			asOf,
		)
		for _, key := range extraTagColumns {
			row = append(row, s.Status.Tags[key])
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

// formatCount writes a count, or nothing when the provider could not report it.
func formatCount(n *int64) string {
	if n == nil {
		return ""
	}
	return strconv.FormatInt(*n, 10)
}

// formatPercent writes a percentage, or nothing when it is unknown.
func formatPercent(p *int32) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(int(*p))
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
