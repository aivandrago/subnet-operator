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

package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// The flags and the environment variable deprecated in 0.9 were removed in 1.0 (owner decision,
// 2026-09-25). Passing one stops the operator with a message that names the replacement,
// instead of it running without its event queue.
func TestRemovedFlagsAreRefusedWithTheirReplacement(t *testing.T) {
	noEnv := func(string) string { return "" }
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want []string
	}{
		{"nothing removed", []string{"--aws-events-queue-url=q", "--aws-events-debounce=5s"}, nil, nil},
		{"--events-queue-url", []string{"--events-queue-url=q"}, nil,
			[]string{"--events-queue-url was removed in 1.0; use --aws-events-queue-url"}},
		{"--events-debounce", []string{"--events-debounce", "5s"}, nil,
			[]string{"--events-debounce was removed in 1.0; use --aws-events-debounce"}},
		{"EVENTS_QUEUE_URL", nil, map[string]string{"EVENTS_QUEUE_URL": "q"},
			[]string{"EVENTS_QUEUE_URL was removed in 1.0; use --aws-events-queue-url"}},
		{"an empty EVENTS_QUEUE_URL", nil, map[string]string{"EVENTS_QUEUE_URL": ""}, nil},
		{"all of them", []string{"--events-debounce=5s", "--events-queue-url=q"},
			map[string]string{"EVENTS_QUEUE_URL": "q"},
			[]string{"--events-queue-url", "--events-debounce", "EVENTS_QUEUE_URL"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("manager", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			fs.String("aws-events-queue-url", "", "")
			fs.Duration("aws-events-debounce", 0, "")
			removed := registerRemovedFlags(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse: %v", err)
			}
			getenv := noEnv
			if tc.env != nil {
				getenv = func(k string) string { return tc.env[k] }
			}
			err := removed.check(getenv)
			if tc.want == nil {
				if err != nil {
					t.Errorf("got %v, want no error", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("got no error, want one naming %v", tc.want)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}
