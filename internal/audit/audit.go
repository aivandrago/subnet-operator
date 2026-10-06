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

// Package audit writes one JSON line per decision that changed, or could have changed,
// somebody's infrastructure: an import, an allocation, an auto-import policy verdict.
//
// It is deliberately not the operational log. The operational log is for whoever is on call
// and changes shape whenever a message is reworded; this stream is a record somebody may have
// to answer questions from months later, so it has a fixed set of fields, goes to its own
// writer and carries nothing else. The fields are documented in docs/audit.md.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Action is what the operator did or decided.
type Action string

const (
	// ActionImport is a ResourceImport: tags applied to take an existing resource over.
	ActionImport Action = "import"
	// ActionAllocate is a SubnetClaim reserving CIDRs and, in Create mode, creating subnets.
	ActionAllocate Action = "allocate"
	// ActionPolicyDecision is one verdict of the auto-import policy about one resource.
	ActionPolicyDecision Action = "policy_decision"
)

// Result is how the action ended. The values match the auto-import metric, so a spike in
// hs_auto_imports_total{result="no_owner"} and the audit lines behind it use one word.
const (
	// ResultApplied means the change reached AWS.
	ResultApplied = "applied"
	// ResultReserved means address space was reserved for a claim; AWS is untouched so far.
	ResultReserved = "reserved"
	// ResultDryRun means the change was computed but deliberately not applied.
	ResultDryRun = "dryrun"
	// ResultSkipped means a policy rule said somebody else owns this resource.
	ResultSkipped = "skipped"
	// ResultNoOwner means no rule resolved the required tags; the resource stays unmanaged.
	ResultNoOwner = "no_owner"
	// ResultFailed means the change was attempted and the cloud refused it.
	ResultFailed = "failed"
)

// CreatedByUnknown is the created_by of a line whose object carries no creator the operator
// can vouch for: it was created while the admission webhooks were off, or before they
// recorded one. It is written out rather than left empty, so that a missing identity reads as
// a gap in the record and not as a field this version of the operator does not have.
const CreatedByUnknown = "unknown"

// Sink modes, as accepted by New.
const (
	// ModeOff writes nothing.
	ModeOff = "off"
	// ModeStdout writes one JSON line per record to stdout, which is not where the
	// operational log goes, so the two streams can be shipped to different places.
	ModeStdout = "stdout"
)

// Record is one audit line. Every field is in docs/audit.md; adding one is a change to a
// contract somebody's SIEM parses, so it belongs in that document too.
type Record struct {
	// Time is when the decision was taken, in RFC 3339 with nanoseconds, UTC.
	Time time.Time `json:"time"`
	// Action is what was decided: import, allocate, policy_decision.
	Action Action `json:"action"`
	// Result is how it ended: applied, dryrun, skipped, no_owner, failed.
	Result string `json:"result"`
	// Scope is the NetworkScope the resource belongs to, empty when the decision was taken
	// on an object that names no scope.
	Scope string `json:"scope,omitempty"`
	// Account and Region locate the resource in the cloud.
	Account string `json:"account,omitempty"`
	Region  string `json:"region,omitempty"`
	// ResourceID is the vpc-…/subnet-… the decision was about. Empty when a claim reserved a
	// CIDR without creating anything yet.
	ResourceID string `json:"resource_id,omitempty"`
	// Object is the Kubernetes object that carries the decision, as kind/namespace/name, so
	// a line can be traced back to what a human applied.
	Object string `json:"object,omitempty"`
	// Principal is who the change is attributed to: the requestedBy of a ResourceImport
	// (which the auto-import policy fills with the CloudTrail creator when it knows one), or
	// the owner of a SubnetClaim. Empty when nothing could be attributed.
	Principal string `json:"principal,omitempty"`
	// CreatedBy is the Kubernetes identity the change is done for, as the API server
	// authenticated it: the user that created the SubnetClaim or ResourceImport (its
	// aws.hypersurgery/created-by annotation), or the operator's own identity for a policy
	// decision. Unlike Principal nobody can choose it. CreatedByUnknown when the operator
	// cannot vouch for it.
	CreatedBy string `json:"created_by,omitempty"`
	// Reason is the policy's own sentence, or the condition message.
	Reason string `json:"reason,omitempty"`
	// TagsBefore are the tags the operator last saw on the resource, TagsAfter the tags it
	// carries after the change. Both are omitted when unknown, which is not the same as empty.
	TagsBefore map[string]string `json:"tags_before,omitempty"`
	TagsAfter  map[string]string `json:"tags_after,omitempty"`
	// CIDR is the block a claim reserved or created.
	CIDR string `json:"cidr,omitempty"`
	// Error is the cloud's refusal, for a failed result.
	Error string `json:"error,omitempty"`
}

// Sink takes audit records. Implementations must be safe for concurrent use: several
// controllers write to the same sink.
type Sink interface {
	Record(ctx context.Context, rec Record)
}

// New builds the sink named by mode. An unknown mode is an error rather than a silent
// fallback: an operator started with a typo must not believe it is keeping an audit trail.
func New(mode string) (Sink, error) {
	switch mode {
	case ModeOff, "":
		return Off{}, nil
	case ModeStdout:
		return NewWriter(os.Stdout), nil
	default:
		return nil, fmt.Errorf("unknown audit sink %q, want %q or %q", mode, ModeOff, ModeStdout)
	}
}

// Off discards every record.
type Off struct{}

// Record does nothing.
func (Off) Record(context.Context, Record) {}

// NewWriter returns a sink writing one JSON line per record to w.
func NewWriter(w io.Writer) Sink {
	return &writerSink{enc: json.NewEncoder(w), now: time.Now}
}

type writerSink struct {
	mu  sync.Mutex
	enc *json.Encoder
	now func() time.Time
}

// Record writes the line. A write that fails is dropped: an audit trail nobody can write is
// worth reporting, but not worth failing a reconcile that already changed AWS.
func (s *writerSink) Record(_ context.Context, rec Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.Time.IsZero() {
		rec.Time = s.now()
	}
	rec.Time = rec.Time.UTC()
	_ = s.enc.Encode(rec)
}

// Emit sends the record to the sink, tolerating a nil one so callers that were built without
// a sink (tests, a manager started with the audit trail off) need no branch of their own.
func Emit(ctx context.Context, s Sink, rec Record) {
	if s == nil {
		return
	}
	s.Record(ctx, rec)
}
