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

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEachRecordIsOneJSONLineWithTheDocumentedFields(t *testing.T) {
	var buf bytes.Buffer
	sink := NewWriter(&buf)

	sink.Record(context.Background(), Record{
		Time:       time.Date(2026, 9, 23, 10, 30, 0, 0, time.UTC),
		Action:     ActionImport,
		Result:     ResultApplied,
		Scope:      "org",
		Account:    "111111111111",
		Region:     "eu-central-1",
		ResourceID: "subnet-0abc1111",
		Object:     "ResourceImport/default/subnet-0abc1111-import",
		Principal:  "policy (created by arn:aws:sts::111111111111:assumed-role/payments-deploy/maria.k)",
		CreatedBy:  "system:serviceaccount:subnet-operator-system:subnet-operator",
		Reason:     "tags from creator rule",
		TagsBefore: map[string]string{"Name": "legacy"},
		TagsAfter:  map[string]string{"Name": "legacy", "hs/managed": "true", "hs/owner": "team-payments"},
	})

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("want one line, got %d: %q", len(lines), buf.String())
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	want := map[string]any{
		"time":        "2026-09-23T10:30:00Z",
		"action":      "import",
		"result":      "applied",
		"scope":       "org",
		"account":     "111111111111",
		"region":      "eu-central-1",
		"resource_id": "subnet-0abc1111",
		"object":      "ResourceImport/default/subnet-0abc1111-import",
		"principal":   "policy (created by arn:aws:sts::111111111111:assumed-role/payments-deploy/maria.k)",
		"created_by":  "system:serviceaccount:subnet-operator-system:subnet-operator",
		"reason":      "tags from creator rule",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("field %s: got %v, want %v", key, got[key], value)
		}
	}
	before, ok := got["tags_before"].(map[string]any)
	if !ok || before["Name"] != "legacy" {
		t.Errorf("tags_before: got %v", got["tags_before"])
	}
	after, ok := got["tags_after"].(map[string]any)
	if !ok || after["hs/owner"] != "team-payments" || after["hs/managed"] != "true" {
		t.Errorf("tags_after: got %v", got["tags_after"])
	}
}

func TestARecordWithoutATimestampGetsOne(t *testing.T) {
	var buf bytes.Buffer
	sink := NewWriter(&buf)

	sink.Record(context.Background(), Record{Action: ActionPolicyDecision, Result: ResultNoOwner})

	var got Record
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if got.Time.IsZero() {
		t.Fatal("want a timestamp on every line, got none")
	}
	if time.Since(got.Time) > time.Minute {
		t.Errorf("timestamp is not the time of the decision: %s", got.Time)
	}
}

func TestUnknownFieldsStayOutOfTheLine(t *testing.T) {
	var buf bytes.Buffer
	NewWriter(&buf).Record(context.Background(), Record{Action: ActionAllocate, Result: ResultApplied})

	line := buf.String()
	for _, absent := range []string{"scope", "account", "region", "resource_id", "tags_before", "tags_after", "error"} {
		if strings.Contains(line, `"`+absent+`"`) {
			t.Errorf("unknown %s should be omitted, not empty: %s", absent, line)
		}
	}
}

func TestConcurrentWritersDoNotInterleaveLines(t *testing.T) {
	var buf bytes.Buffer
	sink := NewWriter(&buf)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			sink.Record(context.Background(), Record{
				Action: ActionPolicyDecision, Result: ResultSkipped, ResourceID: "subnet-0abc1111",
			})
		})
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 50 {
		t.Fatalf("want 50 lines, got %d", len(lines))
	}
	for _, line := range lines {
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line is not JSON: %v (%q)", err, line)
		}
	}
}

func TestTheOffSinkWritesNothing(t *testing.T) {
	sink, err := New(ModeOff)
	if err != nil {
		t.Fatalf("New(off): %v", err)
	}
	// Off has no writer to inspect; what matters is that it is the discarding one.
	if _, ok := sink.(Off); !ok {
		t.Fatalf("want the off sink, got %T", sink)
	}
	sink.Record(context.Background(), Record{Action: ActionImport})
}

func TestAnUnknownSinkNameIsRefusedRatherThanIgnored(t *testing.T) {
	if _, err := New("sylog"); err == nil {
		t.Fatal("want an error for a misspelled sink, got none")
	}
	if _, err := New(ModeStdout); err != nil {
		t.Fatalf("New(stdout): %v", err)
	}
}

func TestANilSinkIsSafeToEmitTo(t *testing.T) {
	Emit(context.Background(), nil, Record{Action: ActionImport})
}
