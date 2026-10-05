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
	"fmt"
	"time"
)

// jitterFactor is client-go's leaderelection.JitterFactor: a renewal is retried at up to 1.2
// times the retry period, so the renew deadline has to leave room for that.
const jitterFactor = 1.2

// validLeaseTimings checks exactly what client-go's leader election will: the leader must give
// up renewing before its lease can expire, or a standby could take a lease the leader still
// believes it holds. client-go checks this too, but deep inside Start and in its own words;
// this names the flags.
func validLeaseTimings(lease, renew, retry time.Duration) error {
	switch {
	case lease <= 0 || renew <= 0 || retry <= 0:
		return fmt.Errorf("leader election durations must be positive: lease %s, renew deadline %s, retry period %s",
			lease, renew, retry)
	case renew >= lease:
		return fmt.Errorf("--leader-elect-renew-deadline (%s) must be shorter than --leader-elect-lease-duration (%s)",
			renew, lease)
	case renew <= time.Duration(jitterFactor*float64(retry)):
		return fmt.Errorf("--leader-elect-renew-deadline (%s) must be longer than %.1f × --leader-elect-retry-period (%s)",
			renew, jitterFactor, retry)
	}
	return nil
}
