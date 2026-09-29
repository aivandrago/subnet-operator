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
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// NewSQSClient returns a client for the events queue, using the operator's own credentials.
// The region comes from a standard queue URL (https://sqs.<region>.amazonaws.com/...) and
// falls back to the default configuration.
func NewSQSClient(ctx context.Context, queueURL string) (*sqs.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	if region := QueueRegion(queueURL); region != "" {
		cfg.Region = region
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("cannot tell the region of queue %q; set AWS_REGION", queueURL)
	}
	return sqs.NewFromConfig(cfg), nil
}

// QueueRegion extracts the region from a standard SQS queue URL, or returns "".
func QueueRegion(queueURL string) string {
	u, err := url.Parse(queueURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(u.Hostname(), ".")
	if len(parts) >= 4 && parts[0] == "sqs" && parts[2] == "amazonaws" {
		return parts[1]
	}
	return ""
}
