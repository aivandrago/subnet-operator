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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/middleware"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// SessionName is the sts:AssumeRole session name, visible in spoke-account CloudTrail.
const SessionName = "aws-subnet-operator"

const (
	// retryMaxAttempts is how often one EC2 call is tried before discovery sees its error. The
	// SDK default of 3 gives up within about a second, which is shorter than a throttling burst
	// on a busy account usually lasts.
	retryMaxAttempts = 5
	// retryMaxBackoff caps the SDK's delay between two attempts of one call. With five attempts
	// a call gives up after about a minute at worst; anything longer is the controller's
	// per-target backoff, which does not hold a discovery slot while it waits.
	retryMaxBackoff = 20 * time.Second
)

// Discoverer discovers targets with real AWS credentials. The base credentials come from
// the default chain (EKS Pod Identity, IRSA, env); spoke accounts are reached via AssumeRole.
type Discoverer struct {
	base aws.Config

	mu          sync.Mutex
	credentials map[string]aws.CredentialsProvider
	retryers    map[inventory.TargetKey]aws.Retryer
	ownAccount  string

	// OnThrottle, when set, is called for every EC2 attempt the API throttled — including the
	// attempts the SDK then retried successfully, which is the early warning: a target can be
	// throttled for a long time before a whole discovery fails.
	OnThrottle func(target inventory.Target, operation string)

	// retryBackoff replaces the SDK's delay between attempts; tests set it to zero.
	retryBackoff retry.BackoffDelayer
	// testEC2, when set, replaces the EC2 client of every call; the contract suite points it
	// at an in-memory EC2.
	testEC2 func(target inventory.Target) ec2Client
}

// ec2Client is every EC2 call the provider makes.
type ec2Client interface {
	EC2API
	EC2WriteAPI
	EC2TagAPI
}

var _ inventory.Discoverer = (*Discoverer)(nil)

// NewDiscoverer loads the default AWS configuration.
func NewDiscoverer(ctx context.Context) (*Discoverer, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return newDiscoverer(cfg), nil
}

// NewDiscovererFromConfig uses the given AWS configuration instead of the default one, for
// an emulator such as Moto.
func NewDiscovererFromConfig(cfg aws.Config) *Discoverer {
	return newDiscoverer(cfg)
}

func newDiscoverer(cfg aws.Config) *Discoverer {
	return &Discoverer{
		base:        cfg,
		credentials: map[string]aws.CredentialsProvider{},
		retryers:    map[inventory.TargetKey]aws.Retryer{},
	}
}

// Discover implements inventory.Discoverer. An error that is still a throttling error after
// the SDK's retries wraps inventory.ErrThrottled.
func (d *Discoverer) Discover(ctx context.Context, target inventory.Target) (*inventory.Snapshot, error) {
	creds, err := d.credentialsFor(ctx, target)
	if err != nil {
		return nil, err
	}
	var api EC2API = d.ec2Client(target, creds)
	if d.testEC2 != nil {
		api = d.testEC2(target)
	}
	snap, err := DiscoverTarget(ctx, api, target)
	if err != nil && isThrottle(err) {
		return nil, fmt.Errorf("%w: %w", inventory.ErrThrottled, err)
	}
	return snap, err
}

// ec2Client builds the EC2 client for one discovery of the target. The client is cheap and
// built every time; the retryer is not, because it carries the adaptive rate limiter's
// measurements of how fast this account and region accept requests, and a fresh one would
// start every sync at full speed again.
func (d *Discoverer) ec2Client(target inventory.Target, creds aws.CredentialsProvider) *ec2.Client {
	cfg := d.base.Copy()
	cfg.Region = target.Region
	cfg.Credentials = creds
	return ec2.NewFromConfig(cfg, func(o *ec2.Options) {
		o.Retryer = d.retryerFor(target.Key())
		// Cloned: the options share the base config's slice, and appending to it in place
		// would race with every other discovery.
		o.APIOptions = append(slices.Clone(o.APIOptions), d.countThrottles(target))
	})
}

// retryerFor returns the retryer of an account/region. EC2 rate-limits per account and region,
// so that is the unit the adaptive mode measures and slows down, whichever scope asks.
func (d *Discoverer) retryerFor(key inventory.TargetKey) aws.Retryer {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r, ok := d.retryers[key]; ok {
		return r
	}
	r := retry.NewAdaptiveMode(func(o *retry.AdaptiveModeOptions) {
		o.StandardOptions = append(o.StandardOptions, func(so *retry.StandardOptions) {
			so.MaxAttempts = retryMaxAttempts
			so.MaxBackoff = retryMaxBackoff
			if d.retryBackoff != nil {
				so.Backoff = d.retryBackoff
			}
		})
	})
	d.retryers[key] = r
	return r
}

// countThrottles reports every throttled attempt to OnThrottle. It sits right after the retry
// middleware, so it runs once per attempt rather than once per call.
func (d *Discoverer) countThrottles(target inventory.Target) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Finalize.Insert(middleware.FinalizeMiddlewareFunc("CountThrottles",
			func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (
				middleware.FinalizeOutput, middleware.Metadata, error) {
				out, md, err := next.HandleFinalize(ctx, in)
				if err != nil && d.OnThrottle != nil && isThrottle(err) {
					d.OnThrottle(target, awsmiddleware.GetOperationName(ctx))
				}
				return out, md, err
			}), "Retry", middleware.After)
	}
}

// isThrottle reports whether the error is AWS rate-limiting the caller (RequestLimitExceeded
// on EC2, Throttling on STS, and the other codes the SDK itself treats as throttling).
func isThrottle(err error) bool {
	return retry.IsErrorThrottles(retry.DefaultThrottles).IsErrorThrottle(err) == aws.TrueTernary
}

// credentialsFor returns cached credentials for the target and makes sure they belong
// to the target account, so a misconfigured scope never reports another account's networks.
func (d *Discoverer) credentialsFor(ctx context.Context, target inventory.Target) (aws.CredentialsProvider, error) {
	id, err := identityOf(target)
	if err != nil {
		return nil, err
	}
	if id.Own() {
		own, err := d.callerAccount(ctx)
		if err != nil {
			return nil, err
		}
		if own != target.Account {
			return nil, fmt.Errorf("account %s has no roleARN and the operator runs in account %s", target.Account, own)
		}
		return d.base.Credentials, nil
	}

	parsed, err := arn.Parse(id.RoleARN)
	if err != nil {
		return nil, fmt.Errorf("parse roleARN: %w", err)
	}
	if parsed.AccountID != target.Account {
		return nil, fmt.Errorf("roleARN %s belongs to account %s, not %s", id.RoleARN, parsed.AccountID, target.Account)
	}

	key := id.RoleARN + "|" + id.ExternalID
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.credentials[key]; ok {
		return p, nil
	}
	p := aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(d.base), id.RoleARN,
		func(o *stscreds.AssumeRoleOptions) {
			o.RoleSessionName = SessionName
			if id.ExternalID != "" {
				o.ExternalID = aws.String(id.ExternalID)
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
