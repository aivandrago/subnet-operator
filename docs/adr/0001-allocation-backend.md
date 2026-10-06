# ADR 0001: CIDR allocation backend

Status: accepted in part. The built-in allocator is implemented (since 0.2.0, unchanged in 1.0);
Amazon VPC IPAM and the tag-defined pools are not implemented and remain open.

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

## Outcome (as of 1.0)

The order was reversed, and the scope narrowed:

- **Built-in, per network.** A `SubnetClaim` names one network (`spec.networkID`), and
  `internal/allocator` picks free blocks inside that network's IPv4 CIDRs: first fit, lowest
  address first, skipping existing subnets and the reservations of other claims. Reservations
  are kept in the claims' `status.allocations`, not in AWS tags. Overlaps *between* networks are
  reported (`hs_network_cidr_overlaps`, the `NetworkCIDROverlap` alert), not prevented: choosing
  the network is the claimant's decision.
- **Races are settled by AWS.** If a reserved block is taken before the subnet is created,
  `CreateSubnet` fails with a conflict, the reservation is dropped and the next pass picks another
  block.
- **No IPAM, no pools, no `Allocator` interface.** The `hs/pool` tag in the README's tag schema is
  not read by the operator. An IPAM backend, or pools that span networks, would be additions to
  the v1 API (a new optional field on `SubnetClaim`), within the
  [compatibility promise](../api-compatibility.md).
