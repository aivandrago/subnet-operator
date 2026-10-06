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

package sheets

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

func subnet(account, region, vpc, id, cidr string) networkv1.Subnet {
	return networkv1.Subnet{
		Spec: networkv1.SubnetSpec{Provider: networkv1.ProviderAWS, ID: id, NetworkID: vpc, Account: account, Region: region},
		Status: networkv1.SubnetStatus{
			CIDRBlock: cidr, Zone: region + "a", TotalIPs: ptr.To[int64](251), AvailableIPs: ptr.To[int64](51),
			UtilizationPercent: ptr.To[int32](79), Owner: "team-a", Env: "prod", Tier: "private",
			AWS: &networkv1.AWSSubnetStatus{RouteTableID: "rtb-1"}, Tags: map[string]string{"cost-center": "cc-42"},
		},
	}
}

func TestBuildTable(t *testing.T) {
	synced := metav1.NewTime(time.Date(2026, 9, 22, 8, 5, 0, 0, time.UTC))
	subnets := []networkv1.Subnet{
		subnet("222222222222", "eu-west-1", "vpc-b", "subnet-b1", "10.1.0.0/24"),
		subnet("111111111111", "eu-central-1", "vpc-a", "subnet-a10", "10.0.10.0/24"),
		subnet("111111111111", "eu-central-1", "vpc-a", "subnet-a9", "10.0.9.0/24"),
	}
	subnets[2].Status.MissingTags = []string{"hs/env", "hs/tier"}

	tbl := BuildTable(subnets,
		map[string]string{"vpc-a": "prod"},
		map[string]*metav1.Time{"111111111111/eu-central-1": &synced},
		[]string{"cost-center"})

	if got, want := tbl.Header[len(tbl.Header)-1], "cost-center"; got != want {
		t.Errorf("last header = %q, want %q", got, want)
	}
	if len(tbl.Rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(tbl.Rows))
	}
	order := []string{tbl.Rows[0][5], tbl.Rows[1][5], tbl.Rows[2][5]}
	want := []string{"subnet-a9", "subnet-a10", "subnet-b1"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("rows sorted %v, want %v (9 before 10, account before region)", order, want)
		}
	}
	row := tbl.Rows[0]
	if row[0] != "AWS" || row[1] != "111111111111" || row[4] != "prod" || row[7] != "10.0.9.0/24" || row[15] != "79" ||
		row[17] != "rtb-1" {
		t.Errorf("unexpected row %v", row)
	}
	if row[16] != "hs/env, hs/tier" {
		t.Errorf("missing tags cell = %q", row[16])
	}
	if row[18] != "2026-09-22 08:05" {
		t.Errorf("data-as-of cell = %q", row[18])
	}
	if tbl.Rows[2][18] != "" {
		t.Errorf("a target without a sync time must leave the cell empty, got %q", tbl.Rows[2][18])
	}
	if last := tbl.Rows[0][len(tbl.Header)-1]; last != "cc-42" {
		t.Errorf("extra tag column = %q, want cc-42", last)
	}
}

// A provider that cannot count the free addresses of a subnet reports none; the sheet must say
// nothing rather than 0, which reads as a full subnet.
func TestBuildTableLeavesUnknownCountsEmpty(t *testing.T) {
	s := subnet("111111111111", "eu-central-1", "vpc-a", "subnet-a", "10.0.0.0/24")
	s.Status.AvailableIPs, s.Status.UtilizationPercent = nil, nil
	row := BuildTable([]networkv1.Subnet{s}, nil, nil, nil).Rows[0]
	if row[13] != "251" || row[14] != "" || row[15] != "" {
		t.Errorf("total, free, used %% = %q, %q, %q; want 251 and two empty cells", row[13], row[14], row[15])
	}
}

func TestCompareCIDR(t *testing.T) {
	if compareCIDR("10.0.9.0/24", "10.0.10.0/24") >= 0 {
		t.Error("10.0.9.0/24 must sort before 10.0.10.0/24")
	}
	if compareCIDR("10.0.0.0/16", "10.0.0.0/24") >= 0 {
		t.Error("the wider prefix sorts first")
	}
	if compareCIDR("bad", "10.0.0.0/8") <= 0 {
		t.Error("unparsable values sort last")
	}
}
