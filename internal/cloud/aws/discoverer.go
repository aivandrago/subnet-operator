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
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// SessionName is the sts:AssumeRole session name, visible in spoke-account CloudTrail.
const SessionName = "aws-subnet-operator"

// Discoverer discovers targets with real AWS credentials. The base credentials come from
// the default chain (EKS Pod Identity, IRSA, env); spoke accounts are reached via AssumeRole.
type Discoverer struct {
	base aws.Config

	mu          sync.Mutex
	credentials map[string]aws.CredentialsProvider
	ownAccount  string
}

var _ inventory.Discoverer = (*Discoverer)(nil)

// NewDiscoverer loads the default AWS configuration.
func NewDiscoverer(ctx context.Context) (*Discoverer, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &Discoverer{base: cfg, credentials: map[string]aws.CredentialsProvider{}}, nil
}

// Discover implements inventory.Discoverer.
func (d *Discoverer) Discover(ctx context.Context, target inventory.Target) (*inventory.Snapshot, error) {
	creds, err := d.credentialsFor(ctx, target)
	if err != nil {
		return nil, err
	}
	cfg := d.base.Copy()
	cfg.Region = target.Region
	cfg.Credentials = creds
	return DiscoverTarget(ctx, ec2.NewFromConfig(cfg), target)
}

// credentialsFor returns cached credentials for the target and makes sure they belong
// to the target account, so a misconfigured scope never reports another account's networks.
func (d *Discoverer) credentialsFor(ctx context.Context, target inventory.Target) (aws.CredentialsProvider, error) {
	if target.RoleARN == "" {
		own, err := d.callerAccount(ctx)
		if err != nil {
			return nil, err
		}
		if own != target.Account {
			return nil, fmt.Errorf("account %s has no roleARN and the operator runs in account %s", target.Account, own)
		}
		return d.base.Credentials, nil
	}

	parsed, err := arn.Parse(target.RoleARN)
	if err != nil {
		return nil, fmt.Errorf("parse roleARN: %w", err)
	}
	if parsed.AccountID != target.Account {
		return nil, fmt.Errorf("roleARN %s belongs to account %s, not %s", target.RoleARN, parsed.AccountID, target.Account)
	}

	key := target.RoleARN + "|" + target.ExternalID
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.credentials[key]; ok {
		return p, nil
	}
	p := aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(d.base), target.RoleARN,
		func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = SessionName
			if target.ExternalID != "" {
				o.ExternalID = aws.String(target.ExternalID)
			}
		}))
	d.credentials[key] = p
	return p, nil
}

func (d *Discoverer) callerAccount(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ownAccount != "" {
		return d.ownAccount, nil
	}
	cfg := d.base.Copy()
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("get caller identity: %w", err)
	}
	d.ownAccount = strings.TrimSpace(aws.ToString(out.Account))
	return d.ownAccount, nil
}
