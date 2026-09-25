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

import "testing"

func TestQueueRegion(t *testing.T) {
	cases := map[string]string{
		"https://sqs.eu-central-1.amazonaws.com/123456789012/aws-subnet-operator-events": "eu-central-1",
		"https://sqs.cn-north-1.amazonaws.com.cn/123456789012/q":                         "cn-north-1",
		"http://10.0.0.5:5000/123456789012/aws-subnet-operator-events":                   "",
		"::bad": "",
	}
	for in, want := range cases {
		if got := QueueRegion(in); got != want {
			t.Errorf("QueueRegion(%q) = %q, want %q", in, got, want)
		}
	}
}
