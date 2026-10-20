# AWS fixed-pool preview

[preview.yaml](preview.yaml) is a public, native CloudFormation template for
creating Relais in your own account. This change creates no AWS resources.
**This is a reviewable preview candidate. Control restart safety (#193) still
blocks public use.** There is no launch link or selected release.

Read [ADR 0011](../../docs/adr/0011-deploy-and-autoscale.md) and the
[deployment guide](../../docs/deploy.md). The template supports only a new,
dedicated IPv4 VPC. It has no vendor access role, existing-VPC option,
autoscaling, lifecycle hooks, MemoryDB, SSH ingress, or key pair.

## Resources and idle footprint

An Availability Zone (AZ) is a separate failure location within one region.
Both profiles use two AZs. These resources exist with **zero calls**:

| Resource | `lean` (default) | `resilience` | Reachability |
| --- | ---: | ---: | --- |
| Worker EC2 instances / encrypted 16-GiB gp3 root volumes | 2 / 2 | 3 / 3 | Private subnets |
| Relay EC2 instance / encrypted root volume | 1 / 1 | 1 / 1 | Public subnet; only UDP 34780 inbound |
| Shared control-and-signaling instance / encrypted root volume | 1 / 1 | 0 | Private subnet |
| Separate control and signaling instances / encrypted root volumes | 0 | 2 / 2 | Private subnets |
| Total EC2 instances / root volumes | 4 / 4 | 6 / 6 | As above |
| Valkey nodes: one shard, primary and replica | 2 | 2 | Private; different AZs |
| Application Load Balancer (ALB) / HTTPS listener / target group | 1 / 1 / 1 | 1 / 1 / 1 | Public 443; private HTTPS targets |
| WAF web ACL / rate rule | 1 / 1 | 1 / 1 | Attached to the ALB |
| NAT gateways / their Elastic IPs | 1 / 1 | 2 / 2 | Public subnets; outbound connections |
| Retained relay Elastic IP / primary network interface | 1 / 1 | 1 / 1 | Fixed caller address; dedicated interface |
| Secrets Manager generated secrets | 5 | 5 | AWS API; exact role grants |
| VPC / public subnets / private subnets / internet gateway | 1 / 2 / 2 / 1 | 1 / 2 / 2 / 1 | Dedicated network |
| Component security groups | 6 | 6 | Relay, worker, control, signaling, ALB, store |
| Store users / user group / subnet group | 4 / 1 / 1 | 4 / 1 / 1 | Default user disabled |
| IAM instance roles / instance profiles | 5 / 5 | 5 / 5 | Only the profile's hosts use them |

NAT provides egress to GitHub release artifacts, Python packages, AWS APIs and
future provider HTTPS APIs. The lean profile routes both private subnets through
one NAT in AZ A. Resilience uses one NAT per AZ. NAT gateways, public IPv4
addresses, ALB, WAF, EC2, EBS, Valkey, secrets and store snapshots are part of
the idle resource inventory. Data transfer, requests and logs can add usage.
No dollar estimate or measured capacity is supplied. NAT's inbound and address
behavior is documented in [AWS NAT gateways](https://docs.aws.amazon.com/vpc/latest/userguide/vpc-nat-gateway.html)
(read 2026-10-19).

## Parameters and initial creation

| Parameter | Selection |
| --- | --- |
| `Profile` | `lean` or `resilience`; default `lean` |
| `ReleaseVersion` | Required stable version after `0.2.0`, without `v`; **no default** |
| `ArchiveSHA256` | Required lowercase SHA-256 of that release's Linux archive for the selected architecture |
| `Architecture` | `arm64` (Graviton default) or `x86_64` (archive suffix `amd64`) |
| `ArmInstanceType`, `X86InstanceType` | Defaults `m7g.large` and `m7i.large`; check regional availability and architecture compatibility |
| `ArmImage`, `X86Image` | AL2023 public SSM image parameter paths; use a reviewed image parameter for repeatable launches |
| `CertificateArn` | Your ACM certificate in the stack region; no certificate or DNS zone is created |
| `EnableSessionManager` | `false` by default; `true` grants the limited SSM agent permissions and enables its service |
| `WorkerCallCap` | Default 2, accepted range 1–8; a refusal boundary, **not** measured EC2 capacity |
| `SignalRateLimit` | Default 100 requests per source IP per five-minute window; minimum 10 |

Select a region with Valkey 9.0, `cache.m7g.large`, synchronous durability, the
EC2 types and at least two ordinary AZs. The template selects the first two
AZs. Review quotas and the complete change set. Use a lowercase stack name of
at most 24 characters because it prefixes named IAM roles and store user IDs.
The stack needs `CAPABILITY_NAMED_IAM`. The template is larger than the inline
API body limit; upload the reviewed local file in the console or use your own
private S3 staging location. Release hosting is a later slice. AWS documents
[AL2023 images](https://docs.aws.amazon.com/linux/al2023/ug/ec2.html),
[CloudFormation template limits](https://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/cloudformation-limits.html),
and [ACM certificates for ALB](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/https-listener-certificates.html)
(read 2026-10-19).

Obtain the archive digest independently from the chosen release's reviewed
checksum manifest. Boot downloads
`https://github.com/relaishq/relais/releases/download/vVERSION/relais_VERSION_linux_ARCH.tar.gz`,
checks its exact SHA-256, rejects unsafe archive members and starts nothing on
a mismatch. No published post-0.2.0 artifact is selected by this template.

Cloud-init installs Python 3.11 and the pinned boto3 SDK, then reads only its
own stack-resource metadata. This metadata holds the exact boot scripts,
systemd units and public settings. It contains secret **references**, not
values. `tests/sync-files.py` checks the embedded copies. After editing a boot
file or unit, run `python tests/sync-files.py --write` from this directory.
OS packages and the default SSM image aliases can change; record the resolved
AMI and OS package inventory in any paid acceptance run. See
[DescribeStackResource](https://docs.aws.amazon.com/AWSCloudFormation/latest/APIReference/API_DescribeStackResource.html)
(read 2026-10-19).

`CREATE_COMPLETE` is not application readiness. This slice has no CloudFormation
success signal. Verify private control `/status` readiness, both workers (three
for resilience), relay registration, ALB target status and an outside test call
before accepting a later deployment. Require actual `EffectiveDurability=sync`.
Do not expose a public preview until #193 and the paid acceptance gates pass.

After a separately approved creation, configure your certificate's DNS name to
the `SignalLoadBalancer` output. Caller media uses `RelayIPv4:34780`. Outputs
contain no token, password, snapshot key or credential-bearing URL.

## Boundaries and secrets

The only public ingress is relay **UDP 34780** and ALB **TCP 443**. HTTPS on
the ALB reaches signaling over private HTTPS 34743. The target's certificate is
self-signed; ALB does not verify backend certificates. Health checks expect
unauthenticated `/status` to return 401, which checks the HTTPS listener, not
control readiness. ALB forwards only `/calls`, `/calls/*`, `/status` and `/clock`;
the existing signaling handler further limits methods and exact paths. WAF uses
the direct source IP, disables sampled requests and blocks excess requests.
Rate enforcement is approximate; clients behind one NAT share a budget.
[ALB target TLS](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/load-balancer-target-groups.html)
and [WAF rate rules](https://docs.aws.amazon.com/waf/latest/developerguide/waf-rule-statement-type-rate-based.html)
describe these limits (read 2026-10-19).

| Source group | Destination group | Allowed private path |
| --- | --- | --- |
| Worker | Relay | UDP 34781, worker leg |
| Relay | Worker | UDP 34711, private media |
| Control | Worker | TCP 34701, worker API |
| Worker | Control | TCP 34710, heartbeats and coordination |
| Control | Relay | TCP 34700, relay API |
| Signaling | Control | TCP 34710, bounded frontend |
| ALB | Signaling | TCP 34743, HTTPS and health checks |
| Relay, worker, control | Store | TCP 6379, TLS and Cluster discovery |

Each path has matching ingress and egress rules. There is no VPC-wide private
API ingress. HTTPS egress is the only internet egress rule. An unusable loopback
ICMP rule suppresses each group's implicit allow-all egress. The VPC DNS
resolver is handled by AWS and is not filtered by security groups. Security
groups are stateful, so responses do not need reverse ingress rules. See
[AWS security-group rules](https://docs.aws.amazon.com/vpc/latest/userguide/security-group-rules.html)
(read 2026-10-19).

Every instance requires IMDSv2 with a one-hop response limit and encrypted EBS.
Application services deny access to the IPv4 metadata address through systemd,
run as separate unprivileged users, have no Linux capabilities and use a read-only
system filesystem. The two relays deliberately share one user, host PID
namespace and binary inode so ADR 0004 fencing can work; they do not hide `/proc`
from each other. Review the actual Linux and AMI behavior in a paid run.
[AWS metadata settings](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-properties-ec2-instance-metadataoptions.html)
and [encrypted EBS mappings](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-properties-ec2-instance-ebs.html)
were read 2026-10-19.

Secrets Manager generates five independent secrets: a 32-byte hex snapshot key,
a signaling token, and relay, worker and control store passwords. CloudFormation
passes password dynamic references only to ElastiCache user resources. At boot,
root fetches exact permitted ARNs and writes root-owned 0600 files under
`/run/relais` (directory 0700). systemd reads each component's `EnvironmentFile`;
signaling uses `LoadCredential` and `-token-file`. Values never enter UserData,
outputs, process arguments or boot logs. SDK errors are replaced with a fixed
failure message. The fake-source switches exist only for the local test; the
stack never sets them. Secrets refresh at host boot; rotation is not automatic.
[AWS generated secrets](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-properties-secretsmanager-secret-generatesecretstring.html)
and [dynamic reference limits](https://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/dynamic-references-secretsmanager.html)
were read 2026-10-19.

| Instance identity | Exact secret reads |
| --- | --- |
| Relay | Relay store password |
| Worker | Worker store password; snapshot key |
| Control | Control store password; snapshot key |
| Signaling | Signaling token |
| Shared lean host | Control password; snapshot key; signaling token |

All identities may describe resources only within their own stack to fetch boot
metadata. The stack ARN ends with `/*` because CloudFormation allocates the stack
ID. This grant exposes public configuration and secret ARNs throughout that
stack, not secret values. No runtime role has `iam:PassRole`, EC2 mutation,
secret enumeration, secret writes or wildcard actions. The only `Resource: '*'`
statements are optional SSM channel actions, which have no resource-ARN scope.
The optional `ssm:UpdateInstanceInformation` grant uses the account/region EC2
instance ARN prefix and requires both this stack's `RelaisStack` tag and the
role's `RelaisRole` tag. Its instance-ID suffix is a documented wildcard because
the instance role must exist before its instance ID is allocated. The parameter removes these
grants and masks the agent by default. Modern SSM uses `ssmmessages`; old
`ec2messages` fallback is not granted. See
[SSM message APIs](https://docs.aws.amazon.com/systems-manager/latest/userguide/systems-manager-setting-up-messageAPIs.html)
and [Systems Manager authorization](https://docs.aws.amazon.com/service-authorization/latest/reference/list_ssm.html)
and [message channel authorization](https://docs.aws.amazon.com/service-authorization/latest/reference/list_ssmmessages.html)
(read 2026-10-19).

Use a separate, customer-controlled CloudFormation execution role. Its
`iam:PassRole` permission for EC2 must list only these five exact role ARNs:
`STACK-relay`, `STACK-worker`, `STACK-control`, `STACK-signal`, `STACK-shared`.
Require `iam:PassedToService: ec2.amazonaws.com`. The principal passing the
CloudFormation execution role must separately name only that execution role and
require `cloudformation.amazonaws.com`. Never grant account-wide PassRole.
The template does not create or broaden your deployment principal. See
[AWS PassRole guidance](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_use_passrole.html)
(read 2026-10-19).

A compromised root process on the shared lean host has that host's combined
IAM scope. Separate service users and metadata denial reduce accidental access;
they do not turn a shared host into separate trust domains. Internal HTTP still
has no peer authentication and can carry key material. This dedicated private
network is the preview trust boundary. No peering or unrelated workloads belong
inside it. Mutual authentication is a later general-release gate.

## Store ACLs and durability

The store explicitly selects Valkey 9.0, Cluster mode **enabled**, one shard,
a primary in AZ A, a replica in AZ B, automatic failover, Multi-AZ,
`Durability: sync`, TLS required and encryption at rest. cfn-lint **1.57.2**
bundles and accepts the durability property; checks fail if it is absent.
Never substitute `async` or `default`. See
[CloudFormation replication groups](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-elasticache-replicationgroup.html)
and [durability prerequisites](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/Durability.Limitations.html)
(read 2026-10-19).

| Store identity | ACL boundary |
| --- | --- |
| `relay` | Lease, route and relay-tenure commands, scans and Lua. No `GET`, `SET`, global epoch allocation, worker indexes or frame keys. No snapshot encryption key. |
| `worker` | Explicit session, encrypted snapshot/frame, index, route and fencing commands under `relais:preview:*`. No administrative commands. |
| `control` | Its own password and the explicit commands for that same namespace. Current control opens the full store; do not claim a read-only identity. |
| `default` | Disabled, no command access |

The current ownership Lua path passes six session keys and deletes expired
snapshot/publication data. Consequently the relay ACL must name `state`,
`record`, `epoch` and `ended` families as well as lease/routes/relay tenure and
allow `DEL` cleanup. This permits deletion of snapshot ciphertext; it **does not**
permit reading it. The narrower Go interface alone is not an ACL guarantee.
Reducing this cleanup privilege needs a later binary change. The local test
uses these exact template access strings and asserts snapshot `GET`, frame
access and administrative commands are refused for relay. ElastiCache RBAC
behavior is described in [AWS RBAC](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/Clusters.RBAC.html)
(read 2026-10-19).

## Operation, restart and deletion limits

There is one relay **host**, including in resilience. Its standby protects
same-host process failure only; a host/AZ failure drops media. Media is UDP
only, with no TURN or TCP/TLS fallback. Lean control and signaling share a
failure. One surviving worker leaves no destination for a further move.
Demo/test registration and SRTP diagnostics are disabled. Boot and service logs
record outcomes; there is no ALB access log or WAF sampled-body log in this slice.

Services use `Restart=no`. A failed control process must not restart itself
against live calls before #193. Boot on host restart starts enabled services,
which also remains unsafe for public use until restart reconciliation lands.
Choose `Profile` at creation; changing it later is unsupported because host IPs
are fixed. This template is for **initial creation**: changing release metadata does not
implement a worker drain, safe replacement, cloud-init replay, or rollback.
Do not treat a stack update as a live rollout. Keep the store namespace, secrets
and relay address stable. Snapshot-key rotation requires compatible migration;
signaling-token rotation requires a coordinated signaling restart. There is no
rotation or live-upgrade procedure claimed by this slice.

`DeletionPolicy: Retain` and `UpdateReplacePolicy: Retain` protect the store,
its ACL users/user group/subnet group, all five secrets and the relay Elastic IP.
The VPC, private subnets and component groups are also retained because the live
store depends on them. Ingress/egress rule resources are removed, so retained
store access must be deliberately restored by its owner. EC2, root volumes,
ALB/WAF, NAT gateways and NAT addresses are deleted. Retained resources leave
stack management; replacement can retain **old and new** copies. Retained
Valkey, secrets, relay IPv4 and any remaining snapshots **keep costing money
after stack deletion**. Inventory them before and after deletion. Explicitly
delete retained store, snapshots, users/groups, secrets, address and network
dependencies only after verifying there are no live calls or needed recovery
data. Deletion is not a cost cap. See
[AWS DeletionPolicy](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-attribute-deletionpolicy.html)
and [UpdateReplacePolicy](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-attribute-updatereplacepolicy.html)
(read 2026-10-19).

## Free CI and local evidence

[deploy-template.yml](../../.github/workflows/deploy-template.yml) runs on PRs
and pushes touching this directory or its workflow, and by manual dispatch.
It has no AWS credentials, SDK credential setup, cloud API calls or cloud spend.
Actions use immutable commit SHAs. Tools are pinned: Python 3.12.12,
cfn-lint 1.57.2, ShellCheck 0.11.0, systemd-analyze 255.4-1ubuntu8.17,
Go 1.26.9 and Redis 7.0.11. Downloaded validator/Redis archives have SHA-256
checks. The runner's systemd runtime must remain major version 255 or the
Linux job fails visibly. Expected total wall time is about 5–8 minutes, not a
measured hosted result.

The template job lints both regional schemas, checks public/private paths,
IAM scope, secrets, IMDSv2, disks, retention and synchronous durability. It
resolves both profiles, both architectures and SSM off/on. Twenty unsafe
mutations must fail. Embedded boot-file parity, shell lint and unit verification
are separate checks.

The boot job installs no Redis system service. Six owned local Redis Cluster
processes use free client/bus ports, avoiding 6379, 7000–7005, 9101 and 9201.
It applies the template's exact per-role ACLs with a separate test-only admin.
The Go #190 fixture requires `testing.T` and has no CLI; this Python-only test
follows its six-node/port-reservation design without editing Go code. Redis
replication remains asynchronous in this local fixture; it cannot prove AWS
synchronous durability. Store TLS compatibility is covered by #190's separate
TLS/Cluster CI lane; this boot test uses loopback plaintext Redis.

A local HTTP server supplies the archive, and a local file supplies fake
secrets. A wrong digest must stop the **install script** before any service
starts. Valid boot uses a minimal tool PATH. Linux then runs the shipped units
with test-only path/address drop-ins and the real hardening settings. A real
`relais-load` caller authenticates through signaling and completes a four-second
audio call; at least 50 decrypted audio packets and zero decryption failures
are required. Separate wrong-ACL and wrong-unit runs must return the specific
expected-refusal code. No failure is accepted merely because a test exited
nonzero. No fake secret files or full caller reports are uploaded.

On macOS, run the same unit commands as plain processes:

```sh
python3.12 -m venv /your/scratch/venv
/your/scratch/venv/bin/pip install -r deploy/aws/tests/requirements.txt
/your/scratch/venv/bin/cfn-lint -r us-east-1 us-west-2 -t deploy/aws/preview.yaml
/your/scratch/venv/bin/python deploy/aws/tests/policy.py
/your/scratch/venv/bin/python deploy/aws/tests/sync-files.py
shellcheck deploy/aws/boot/*.sh deploy/aws/tests/*.sh
go build -o /your/scratch/bin/ ./cmd/relais-relay ./cmd/relais-worker ./cmd/relais-control ./cmd/relais-signal ./cmd/relais-load
/your/scratch/venv/bin/python -u deploy/aws/tests/boot-test.py \
  --bin /your/scratch/bin --work /your/scratch/new-trial-directory
```

The trial directory must not exist. It contains private fake credentials and
reports; keep it private and remove it after inspection. The harness stops only
its recorded children. Mac checks cannot show systemd credential delivery,
Linux hardening, pidfd behavior or namespace isolation. CI cannot establish AWS
provisioning, IAM enforcement, real ACM/DNS/ALB/WAF behavior, regional quotas,
EIP routing, managed failover durability, capacity, upgrade/rollback or cleanup.
Those require a separately approved, capped paid AWS acceptance run. No AWS
account was used to author or locally test this template.
