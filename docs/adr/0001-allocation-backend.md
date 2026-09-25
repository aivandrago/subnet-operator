# ADR 0001: CIDR allocation backend

Status: proposed

## Context

The operator must allocate non-overlapping CIDRs across multiple accounts and regions.
The operator runs in one hub account and assumes roles in spoke accounts.

## Options

1. **Amazon VPC IPAM.** Org-wide pools (delegated admin account, pools shared via RAM),
   built-in overlap and utilization tracking, `CreateSubnet` can take an IPAM pool directly.
   Cost: Advanced tier is billed per active IP address. Check the current price for our IP count.
2. **Built-in allocator.** Free CIDR search over VPC pools defined by tags, state kept in AWS tags.
   Free and cloud-agnostic, but we own correctness (races, overlaps across accounts).

## Proposal

Define an `Allocator` interface. Implement IPAM first for AWS; keep the built-in allocator for
environments without IPAM and as the base for GCP/Azure.
