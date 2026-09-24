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
	"context"
	"testing"
	"time"

	"k8s.io/client-go/tools/leaderelection"
)

// The flags are checked before the manager starts so a bad combination fails with a message
// naming the flag. That is only worth anything if the check agrees with client-go's own, so
// every case is also put to client-go and the two answers must match.
func TestLeaseTimingsAgreeWithClientGo(t *testing.T) {
	s := time.Second
	cases := []struct {
		name                string
		lease, renew, retry time.Duration
	}{
		{"client-go defaults", 15 * s, 10 * s, 2 * s},
		{"a long lease for a busy API server", 60 * s, 40 * s, 5 * s},
		{"renew deadline equal to the lease", 15 * s, 15 * s, 2 * s},
		{"renew deadline longer than the lease", 10 * s, 15 * s, 2 * s},
		{"retry period just too close to the deadline", 15 * s, 10 * s, 9 * s},
		{"retry period exactly at the jitter bound", 15 * s, 12 * s, 10 * s},
		{"zero lease", 0, 10 * s, 2 * s},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ours := validLeaseTimings(c.lease, c.renew, c.retry)
			_, theirs := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
				LeaseDuration: c.lease, RenewDeadline: c.renew, RetryPeriod: c.retry,
				Lock: nil,
				Callbacks: leaderelection.LeaderCallbacks{
					OnStartedLeading: func(context.Context) {}, OnStoppedLeading: func() {}},
			})
			// client-go also refuses a nil lock; that is not a timing, so it does not count.
			if theirs != nil && theirs.Error() == "Lock must not be nil." {
				theirs = nil
			}
			if (ours == nil) != (theirs == nil) {
				t.Fatalf("ours says %v, client-go says %v", ours, theirs)
			}
		})
	}
}
