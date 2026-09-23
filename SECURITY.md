# Security

## Reporting a vulnerability

Please report privately rather than in a public issue: **security@hypersurgery.dev**.

Include what the operator did, what an attacker could reach, and how to reproduce it. A
deployment you can share the shape of (a redacted `NetworkScope`, the IAM policies in use) helps
more than a stack trace. We aim to acknowledge within three working days and to agree a
disclosure date with you once the fix is understood; credit is yours unless you ask otherwise.

## What the operator can do, by design

Worth knowing when judging an issue's severity:

- **Read paths never write.** Discovery calls `ec2:DescribeVpcs`, `ec2:DescribeSubnets` and
  `ec2:DescribeRouteTables`, nothing else, through a read-only role per account.
- **Writes are off unless asked for twice**: the manager needs `--enable-writes`, and the
  account needs a `writeRoleARN` separate from the discovery role. Tagging an existing resource
  needs only `ec2:CreateTags`; creating subnets needs `ec2:CreateSubnet` and friends.
- **Nothing is ever deleted.** There is no code path that deletes a VPC, a subnet or a tag in a
  cloud account. Deleting a `SubnetClaim` or a `ResourceImport` leaves AWS untouched.
- **Credentials are never logged**, and role ARNs are checked against the account ID declared in
  the scope, so a mistyped ARN reports an error rather than reading another account.
- **Google credentials** for the Sheet export must be a service account key; other credential
  configurations are rejected because they can make the Google client execute a local command.

## Things that are not vulnerabilities

- The operator mirrors resource tags into cluster objects. Anyone who can read `Subnet` objects
  can read those tags — put nothing secret in tags, and scope RBAC accordingly.
- Metrics carry account IDs, region names and resource IDs by design; protect the metrics
  endpoint as you would any other operational data (the chart serves it over HTTPS with
  authentication by default).

## Supported versions

Pre-1.0: fixes land on `master` and in the next tagged release. There are no backports yet.
