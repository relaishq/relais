# AWS fixed-pool preview

[preview.yaml](preview.yaml) is a public, native CloudFormation template for
creating Relais in your own account. This change creates no AWS resources.
**This is a reviewable preview candidate. Single-control restart recovery (#193)
is tested locally for the fixed-pool topology on standalone Redis and Redis
Cluster. Paid AWS acceptance still blocks public use.** There is no launch link
or selected release.

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
| Relay EC2 instance / encrypted root volume | 1 / 1 | 1 / 1 | Public subnet; only UDP 24780 inbound |
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
| Store role users / user group / subnet group | 3 / 1 / 1 | 3 / 1 / 1 | Valkey disables default user automatically |
| Explicit subnet address reservations / metadata handle | 6 / 1 | 6 / 1 | No interfaces or running compute |
| IAM instance roles / instance profiles | 5 / 5 | 5 / 5 | Only the profile's hosts use them |

NAT provides egress to GitHub release artifacts, AWS APIs and
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
| `ImageId` | Required explicit **standard AL2023 AMI ID** matching `Architecture`; no default or changing SSM alias |
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

Cloud-init uses the standard AL2023 AMI's AWS CLI v2, system Python and
`aws-cfn-bootstrap`. It downloads no OS packages, Python SDK or PyPI dependency.
Select a standard AMI containing these tools, `curl`, `shadow-utils` and `sysctl`;
minimal/custom images are unsupported. The package inventory is documented by
[AWS AL2023 standard AMI packages](https://docs.aws.amazon.com/linux/al2023/ug/amzn2-al2023-ami.html)
(read 2026-10-20). A fixed AMI ID avoids SSM alias re-resolution replacing all
hosts on an unrelated update. Review the selected AMI and package inventory in
a paid acceptance run. Changing `ImageId` is a host replacement; fixed IPs make
create-before-delete replacement unsafe. Use a separately reviewed, drained new
stack and explicit recovery/migration plan; this preview has no safe live AMI
update procedure.

Boot reads common public metadata once from `BootFiles`, a zero-compute
CloudFormation metadata handle, plus only `node.json` and `node.env` from its own
resource. Boot files and units have one shared copy (each host repeats only the small
UserData entry point); only assigned units and
binaries are installed. `relais-load` is never installed on a production host.
`tests/sync-files.py` checks both embedded content and each UserData loader.
After editing a boot file or unit, run `python tests/sync-files.py --write`
from this directory. The loader explicitly makes executable directories 0755
and `/etc/relais` 0700. Before services start, it applies a sysctl.d file raising
both UDP buffer maxima to 16 MiB. See
[DescribeStackResource](https://docs.aws.amazon.com/AWSCloudFormation/latest/APIReference/API_DescribeStackResource.html)
(read 2026-10-20).

Every host has a 15-minute `CreationPolicy`. UserData sends `cfn-signal` success
only after digest verification, secret preparation, active units and local
readiness checks (including control's fixed-worker bootstrap). A failure signals
failure, and a missing signal times out, causing creation rollback. Each role's
`cloudformation:SignalResource` grant covers only its own stack ARN. The
local test checks both success and failure signals. A signal does not establish
ALB health, outside reachability or AWS durability. Verify ALB target status and
an outside test call, and require actual `EffectiveDurability=sync`, in paid
acceptance. Do not expose a public preview until #193 and those gates pass. See
[CreationPolicy](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-attribute-creationpolicy.html)
and [cfn-signal](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/cfn-signal.html)
(read 2026-10-20).

After a separately approved creation, configure your certificate's DNS name to
the `SignalLoadBalancer` output. Caller media uses `RelayIPv4:24780`. Outputs
contain no token, password, snapshot key or credential-bearing URL.

## Boundaries and secrets

The only public ingress is relay **UDP 24780** and ALB **TCP 443**. HTTPS on
the ALB reaches signaling over private HTTPS 24743. The target's certificate is
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
| Worker | Relay | UDP 24781, worker leg |
| Relay | Worker | UDP 24711, private media |
| Control | Worker | TCP 24701, worker API |
| Worker | Control | TCP 24710, heartbeats and coordination |
| Control | Relay | TCP 24700, relay API |
| Signaling | Control | TCP 24710, bounded frontend |
| ALB | Signaling | TCP 24743, HTTPS and health checks |
| Relay, worker, control | Store | TCP 6379, TLS and Cluster discovery |

Each path has matching ingress and egress rules. There is no VPC-wide private
API ingress. HTTPS egress is the only internet egress rule. An unusable loopback
ICMP rule suppresses each group's implicit allow-all egress. The VPC DNS
resolver is handled by AWS and is not filtered by security groups. Security
groups are stateful, so responses do not need reverse ingress rules. See
[AWS security-group rules](https://docs.aws.amazon.com/vpc/latest/userguide/security-group-rules.html)
(read 2026-10-19).

Component ports use 24700–24781, below Linux’s default ephemeral range
32768–60999. The policy check rejects any unreserved component port within
that range. This avoids outgoing boot connections taking a service port before
its listener binds. No ephemeral-port reservation is needed; UDP buffer limits
are configured separately as described above.

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
`/run/relais` (directory 0700). The public `/opt/relais`, `boot/` and `bin/`
directories have mode 0755 so service users can traverse them. Credential
directories and files keep their restricted modes. systemd reads each
component's `EnvironmentFile`;
signaling uses `LoadCredential` and `-token-file`. Values never enter UserData,
outputs, process arguments or boot logs. Secret-fetch errors are replaced with a fixed
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

All identities may describe resources for boot metadata and signal outcomes only
within their own stack. The stack ARN ends with `/*` because CloudFormation allocates the stack
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
| `relay` | Selectors separate Lua key declaration, ownership/route reads, relay-tenure/route writes and lifecycle cleanup deletes. Cannot directly write a worker lease, read snapshot/frame ciphertext, allocate epochs or load scripts. No snapshot encryption key. |
| `worker` | Explicit session, encrypted snapshot/frame, index, route and fencing commands under `relais:preview:*`, including component `TYPE`/`HEXISTS` and cold-node `SCRIPT LOAD`. No administrative command category. |
| `control` | Its own password and the same explicit namespace commands. Current control opens the full store; do not claim a read-only identity. |

Exactly three password users are attached. There is no custom `default` user:
Valkey groups accept password/IAM users and automatically disable the default
user when attached. The policy pins each access string to its exact token list,
the attached user group and its three users. The local Cluster applies these
strings unchanged (selectors are single ACL arguments).

Relay lifecycle reads declare lease/state/record/epoch/routes/ended keys and the
component families `setup`, `checkpoint`, `work` and `history`. The read/prune
script may delete expired session data. The Lua selector permits those declared
keys; separate command selectors allow ownership/route reads, route and relay
lease hash writes, and the required cleanup `DEL`. Permission to declare a Lua
key does not grant arbitrary inner commands on it. A relay cannot `HSET` or `DEL` a
worker lease (including from arbitrary Lua), `GET` a snapshot, read frame keys, run `CONFIG` or `SCRIPT LOAD`.
Cleanup can delete snapshot ciphertext; this is an explicit remaining privilege.
The local test checks denials on all Cluster nodes and kills the primary during
a live call to require route restoration and continuing audio. Narrowing these
writes uses selectors and requires no binary change. See
[AWS RBAC and selectors](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/Clusters.RBAC.html)
(read 2026-10-20).

## Operation, restart and deletion limits

There is one relay **host**, including in resilience. Its standby protects
same-host process failure only; a host/AZ failure drops media. Media is UDP
only, with no TURN or TCP/TLS fallback. Lean control and signaling share a
failure. One surviving worker leaves no destination for a further move.
Demo/test registration and SRTP diagnostics are disabled. Boot and service logs
record outcomes; there is no ALB access log or WAF sampled-body log in this slice.

Services use `Restart=no`. For relay and standby this follows ADR 0004's bounded
same-host process model: the waiting standby fences and takes over once; systemd
does not race it with a restarted plain primary or invent standby replenishment.
After takeover, redundancy needs deliberate operator recovery. Control can be
restarted against live calls after its predecessor exits, with the same fixed
worker endpoints, store namespace and encryption key. It waits for its store
lease and rebuilds before accepting mutations; see [control restart](../../docs/deploy.md#control-restart).
Services still use deliberate operator restart. Boot on host restart starts
enabled services. This local restart evidence does not validate AWS host boot,
managed-store failover or paid preview acceptance.
Choose `Profile` at creation; changing it later is unsupported because host IPs
are fixed. This template is for **initial creation**: changing release metadata does not
implement a worker drain, safe replacement, cloud-init replay, or rollback.
Do not treat a stack update as a live rollout. Keep the store namespace, secrets
and relay address stable. Snapshot-key rotation requires compatible migration;
signaling-token rotation requires a coordinated signaling restart. There is no
rotation implemented by stack updates. Use the offline release gate below for an
operator-controlled live rollout; it does not automate the CloudFormation change.

### Compatibility gate before upgrade or rollback

Every new release archive includes `compatibility.json` and `relais-compat`.
The archive SHA-256 authenticates this manifest together with its binaries when
compared with an independently reviewed release checksum. Boot checks required
members, manifest version/platform and every binary's linked version/full commit
before installation or service start. Pre-manifest releases are refused; there
is no inferred support for v0.2.0. For #251, use two builds from main that both
carry manifests.

Before **any** drain, process stop, replacement or new standby launch, collect
`pool.json` offline. A live inventory collector remains a follow-up. Required fields:

| Field | Operator source |
| --- | --- |
| `inventory_version: 1`, `complete: true` | Attest that the inventory covers every process in this pool, including active **and** standby relay, all workers, control and signaling. Unknown/unreachable inventory is a refusal. |
| `target_worker_profile` | Reviewed target service arguments: `legacy` with new split admission off, or `split` with it on. This is not proof that old v10 calls disappeared. |
| Each participant's unique `name` and `role` | Fixed stack host/service inventory and installed unit definitions. Role is `worker`, `relay`, `control` or `signal`; name each relay process separately. |
| `profile` | Installed arguments/configuration: worker `legacy` or `split` from `-agent-split-checkpoints`; other roles `default`. Read configuration without exposing credentials. |
| `default_budgets` | For workers, attest `true` only when snapshot size/rate/burst and move-memory settings retain the defaults for that profile. Overrides or unknown settings refuse; their compatibility needs separate evidence. Other roles use `false`. |
| `version`, `commit` | Run that installed binary with `-version`. Commit must be the full SHA and must match its manifest. Do this on each host; cached control status does not report build identity. |
| `archive_sha256` | Original independently verified release checksum/install record for that host's archive. |
| `manifest` | Embed that installed `/opt/relais/bin/compatibility.json` JSON object. |
| `agents` | Each worker's configured registration names, including `echo` for the standard preview; other roles use `[]`. A custom build needs a reviewed manifest with registration identity, declared limits and readable/writable app schemas. Unknown custom agents refuse. |

The envelope is `{"inventory_version":1,"complete":true,"target_worker_profile":"legacy","participants":[...]}`.
Each participant has `name`, `role`, `profile`, `version`, `commit`,
`archive_sha256`, `agents`, `default_budgets` and the embedded `manifest` fields listed above.
All participants must use the target archive's OS/architecture; heterogeneous
platform rollout is outside this first checker.

Verify/extract the target archive into a staging directory. Using its CLI:

```sh
./staged/relais-compat preflight --running pool.json \
  --target staged/compatibility.json --operation upgrade > upgrade-verdict.json
# For rollback, collect a fresh inventory and name the older target:
./staged/relais-compat preflight --running pool.json \
  --target older/compatibility.json --operation rollback > rollback-verdict.json
```

Exit zero and `allowed:true` authorize only the compatibility portion of the
reviewed plan. A refusal exits nonzero with a typed code/reason and emits no
steps. The CLI makes no network or process changes. Do not continue after a
refusal. Serialize the operation and re-collect/recheck after any inventory,
release, agent or configuration change; this gate is not runtime peer enforcement.

The checker evaluates every intermediate state. Upgrade replaces workers first,
then relay (including standby), control and signaling (ADR 0009 also permits
control before relay). Rollback reverses the role order: signaling, control,
relay (including standby), then workers. Keep the newer workers until the
coordination processes have reverted, and follow the emitted order. Every reader must decode and continue the existing state/lifecycle and
new writes; disabling split admission does not make v10 safe for a v9-only target.
All state-reading/mutating participants must have full v10 reader, writer and
lifecycle support **before** activating new split calls (ADR 0012 §4). Do not
activate split admission while rolling the bridge for the first time.

After the gate, freeze admissions and confirm survivor capacity. Apply the
per-role procedures below in the emitted upgrade or rollback order. Lean rollout
requires existing load to fit one worker and has no protection from a further
worker failure; pause when capacity is insufficient. Pre-drain one worker while
it stays running; verify local sessions, authoritative ownership, reservations
and pending transfers are empty. Then stop it, wait until control reports `dead`,
`recovering=false` and zero old calls, call `/private/workers/{name}/replace`,
and start/rejoin that address. Repeat one worker at a time. Stage a compatible
relay standby, stop the active process, verify the successor epoch/routes, then
replenish standby. Replace control only after its predecessor exits and wait
for reconciliation readiness. Keep namespace, keys and relay addresses stable.
These steps retain the existing drain/rejoin/relay safety rules; the compatibility
CLI does not execute or prove them.

Supported versions come from `scripts/release/compatibility-policy.json`: the
newer release explicitly lists its tested peer versions and the oldest live peer
and rollback target. `self` is stamped to the archive version. The initial policy
permits only the same immutable build. Before shipping a second supported release,
record the tested older version and floors in that policy, run both upgrade and
rollback tests, and regenerate `scripts/release/compatibility.json`. A floor alone
never authorizes an unlisted version; semver proximity supplies no evidence.
`0.0.0-dev` templates/manual CI artifacts can carry that policy but cannot
authorize a live rollout. Stamp each #251 build with its own real release version.
For example, build the first main revision as `0.3.0`; before building the second
as `0.3.1`, nominate `0.3.0` in `tested_peers: ["self", "0.3.0"]` and set
both floors to `0.3.0`. Regenerate the template, then build and test that candidate
pair in both directions before publishing or using its allowed verdict. Without
this policy change, two different main builds refuse with `unsupported_pair`.
Nominating a peer records a compatibility claim; it is not test evidence.

Contributor contract updates: private HTTP fixtures include real registered
routes, decoder types, query names (`worker`, `to`, `agent-session`), status codes,
failure codes, and anonymous error bodies. Bump `privateapi.ContractVersion`, then
run `RELAIS_UPDATE_HTTP_GOLDEN=1 go test ./internal/privateapi -run TestPrivateHTTPContractGolden`.
Keep previous versioned fixtures. Redis script discovery follows the transitive
imports of `relais-worker`, `relais-control`, `relais-relay` and `relais-signal`,
including all non-test Go files across build tags. Diagnostic binaries such as
`relais-load` are excluded unless imported by a component. `redis.NewScript`,
`Eval`/`EvalSha` and `EvalRO`/`EvalShaRO` calls are inventoried; raw `Do`/`Process`
script/function commands refuse. Unresolved
dynamic scripts fail generation. Run `RELAIS_UPDATE_STORE_GOLDEN=1 go test ./pkg/sessionstore -run TestStoreScriptContractGolden`
to refresh SHA entries. Bump the store contract version, or record a reviewed note
in each changed entry that persisted state **and wire behaviour** are unchanged.
Redis key and UDP frame changes require their own version bumps and versioned
fixtures (`RELAIS_UPDATE_KEY_GOLDEN=1` and `RELAIS_UPDATE_UDP_GOLDEN=1` on the
corresponding contract tests). Regenerate with
`go run ./scripts/release/compat generate --output scripts/release/compatibility.json`,
then run `go run ./scripts/release/compat check --base BASE_COMMIT`.
The unfiltered CI compatibility job is part of the required `build` aggregate.
Manifest schema 2 adds key-layout and UDP contracts; unknown newer manifests
refuse as `unsupported_manifest`. Use the target archive's CLI. Binary identity
reads have a 10-second deadline per binary before installation can proceed.

Rollback applies the same check to a fresh running inventory. It conservatively
retains formats that running builds can continue, even with new admission off.
It refuses a rollback across an unreadable state format or incomplete lifecycle;
CloudFormation rollback cannot undo agent working/history writes. There is no
downconversion, truncation, stale legacy checkpoint fallback or force flag. Going
farther back requires a separately verified empty incompatible pool and its own
procedure; this initial live checker cannot certify that exception.

For contract maintenance, bump `sessionstore.ScriptContractVersion` for an
incompatible script/store change. A script-only optimization may instead update
its SHA golden with a new note explaining why persisted tuples are unchanged
(ADR 0012 #238). Bump `privateapi.ContractVersion` and add a new versioned
request/response fixture when the private HTTP fixture changes. Generate and check:

```sh
go run ./scripts/release/compat generate --output scripts/release/compatibility.json
go run ./scripts/release/compat check
```

CI compares the manifest with production version values and goldens, checks
contract evolution against the PR base, and checks all four packaged archives.
Golden payload tests cover production request/reply types and endpoint inventory;
there are no whole-source fingerprints. The existing v10/header literals are
read at their specific encoding sites without changing runtime behavior.
Free tests establish offline decisions and boot refusal; loaded AWS upgrade,
rollback, capacity and managed-store durability remain **NOT-RUN** until separately
approved acceptance evidence is recorded.

`DeletionPolicy: RetainExceptOnCreate` and `UpdateReplacePolicy: Retain` protect
the store, its subnet/security groups, all five secrets and the relay Elastic IP.
The retained store also requires its VPC, two private subnets, three ACL users
and user group, so these dependencies use the same policies. Component security
groups, standalone ingress/egress rules, EC2/root volumes, ALB/WAF, public subnets,
NAT gateways and NAT addresses are deleted. Owner access to the retained store
must be deliberately restored after its component rules disappear.

A rollback of the operation that initially created a resource deletes it rather
than leaving empty retained resources and same-name user IDs. This includes a
create that fails its boot signal. Verify rollback actually finishes; failed
AWS deletions still require operator cleanup. After a successful stack is later
deleted, retained resources leave stack management and keep costing money:
Valkey, secrets, relay IPv4 and snapshots remain billable. Replacement can retain
old and new copies. Each such retained store also keeps one VPC and its private
subnets, consuming regional VPC quota. Inventory retained resources before
retrying or deleting. A new stack with the same name will collide with retained
user/group IDs; use a new name or deliberately remove/import the old resources
after reviewing recovery needs. Stack deletion is not a cost cap. See
[AWS DeletionPolicy](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-attribute-deletionpolicy.html),
[UpdateReplacePolicy](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-attribute-updatereplacepolicy.html)
and [user group lifecycle](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/Clusters.RBAC.html)
(read 2026-10-20).

Six explicit `/32` subnet CIDR reservations protect the fixed component
addresses before EC2, ALB, NAT or store interfaces are allocated. Every interface
consumer depends on their creation. They reserve only automatic allocation;
explicitly supplied host addresses can still use them. See
[AWS subnet reservations](https://docs.aws.amazon.com/vpc/latest/userguide/subnet-cidr-reservation.html)
and [CloudFormation reservation properties](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-ec2-subnetcidrreservation.html)
(read 2026-10-20).

## Free CI and local evidence

[deploy-template.yml](../../.github/workflows/deploy-template.yml) runs on PRs
and pushes to `main` touching this directory, its workflow, `cmd/**`, `pkg/**`,
`internal/**`, `go.mod` or `go.sum`, and by manual dispatch.
It has no AWS credentials, SDK credential setup, cloud API calls or cloud spend.
Actions use immutable commit SHAs. Tools are pinned: Python 3.12.12,
cfn-lint 1.57.2, ShellCheck 0.11.0,
Go 1.26.9 and Redis 7.0.11. Downloaded validator/Redis archives have SHA-256
checks; all Python requirements are hash-locked (`pip --require-hashes`). Unit
verification uses the runner's own `systemd-analyze` and logs its version. Expected total wall time is about 5–8 minutes, not a
measured hosted result.

The template job lints both regional schemas, checks public/private paths,
IAM scope, secrets, IMDSv2, disks, retention and synchronous durability. It
resolves both profiles, both architectures and SSM off/on. Thirty-seven named unsafe
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
secrets behind a fake AWS CLI. A wrong digest must stop the **real UserData
loader** before any service starts and send a failure signal. Valid boot sends
success, checks each host role image and uses a minimal tool PATH. Linux then runs the shipped units
with test-only path/address drop-ins and the real hardening settings. A real
`relais-load` caller authenticates through signaling and runs a ten-second
audio call. The primary is killed after live routes are persisted; the standby
must restore those routes and forward at least 50 more packets before the call
ends, with zero decryption failures and no ICE restart. Any Redis permission
error in any unit log fails the test, including errors during cleanup. Separate wrong-ACL and wrong-unit runs must return the specific
expected-refusal code in systemd mode. The unit mutation hides the binary with
`InaccessiblePaths`, exercising hardening rather than replacing ExecStart. No failure is accepted merely because a test exited
nonzero. Fast boot-contract checks reject owner-only executable directories and
unsafe component ports. On systemd failure, the test prints each Relais unit's
status, journal and installed unit text, listening sockets, service identities,
and file ownership/modes. Exact generated secret values, credential assignments
and bearer tokens are redacted before printing or writing reports. Credential
environment-file contents are never dumped. The workflow uploads only the
redacted `relais-boot-diagnostics` artifact on failure; fake secret files and full
caller reports are never uploaded.

On macOS, run the same unit commands as plain processes:

```sh
python3.12 -m venv /your/scratch/venv
/your/scratch/venv/bin/pip install --require-hashes -r deploy/aws/tests/requirements.txt
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
