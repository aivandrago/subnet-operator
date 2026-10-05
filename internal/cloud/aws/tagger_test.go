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

package aws

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// fakeTagger records the CreateTags call and can fail it.
type fakeTagger struct {
	in    *ec2.CreateTagsInput
	calls int
	err   error
}

func (f *fakeTagger) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	f.in = in
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &ec2.CreateTagsOutput{}, nil
}

func TestApplyTags(t *testing.T) {
	f := &fakeTagger{}
	tags := map[string]string{"hs/owner": "team-data", "hs/managed": "true", "Name": "lakehouse"}
	if err := ApplyTags(context.Background(), f, "subnet-04d1c2b3", tags); err != nil {
		t.Fatal(err)
	}

	if got := f.in.Resources; !slices.Equal(got, []string{"subnet-04d1c2b3"}) {
		t.Errorf("resources = %v, want the one subnet", got)
	}
	keys := make([]string, 0, len(f.in.Tags))
	values := make([]string, 0, len(f.in.Tags))
	for _, tag := range f.in.Tags {
		keys = append(keys, aws.ToString(tag.Key))
		values = append(values, aws.ToString(tag.Value))
	}
	// Sorted keys keep the request body stable, which makes logs and tests readable.
	if want := []string{"Name", "hs/managed", "hs/owner"}; !slices.Equal(keys, want) {
		t.Errorf("tag keys = %v, want %v", keys, want)
	}
	if want := []string{"lakehouse", "true", "team-data"}; !slices.Equal(values, want) {
		t.Errorf("tag values = %v, want %v", values, want)
	}
}

func TestApplyTagsWithoutTagsCallsNothing(t *testing.T) {
	f := &fakeTagger{}
	if err := ApplyTags(context.Background(), f, "vpc-0abc", nil); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 {
		t.Errorf("calls = %d, want none: there is nothing to apply", f.calls)
	}
}

func TestApplyTagsWrapsTheError(t *testing.T) {
	sentinel := errors.New("UnauthorizedOperation")
	f := &fakeTagger{err: sentinel}
	err := ApplyTags(context.Background(), f, "subnet-0dead", map[string]string{"hs/owner": "x"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap the API error", err)
	}
	if got := err.Error(); !strings.Contains(got, "subnet-0dead") {
		t.Errorf("error = %q, want the resource named in it", got)
	}
}
