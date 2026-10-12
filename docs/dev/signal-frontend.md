# Test signaling frontend

`relais-signal` lets test callers use a small authenticated HTTPS surface in
front of a private control plane. It starts no relay, worker, store or browser.
Start those services separately. `make build` produces `bin/relais-signal`.

## Start with a per-run certificate

Use an explicit private or loopback bind IP and a nonzero TCP port. There is no
default listener. The control URL must be an HTTP(S) origin with a literal
private or loopback IP. Paths, queries, credentials and fragments are refused.
The command does not use environment HTTP proxies or follow upstream redirects.

For a local test with control listening at `127.0.0.1:9200`:

```sh
run_dir="$(mktemp -d)"
export RELAIS_SIGNAL_TOKEN="$(openssl rand -hex 32)"
./bin/relais-signal \
  -listen=127.0.0.1:9443 \
  -control=http://127.0.0.1:9200 \
  -advertise-ips=127.0.0.1 \
  -cert-out="$run_dir/frontend.pem"
```

For another host, choose its private listener IP and reachable HTTPS port, and
put the IPs callers use in `-advertise-ips`. The comma-separated list supports
IPv4 and IPv6. Binding port 443 can require OS privileges; choose an unprivileged
port for local tests.

Each process creates a fresh self-signed ECDSA P-256 certificate, valid for
24 hours, with the configured IP SANs (Subject Alternative Names). The private
key stays in memory. `-cert-out` optionally writes only the public PEM
certificate with mode 0644 (subject to the process umask) and refuses an
existing path. The startup line prints the listener
and the leaf certificate's SHA-256 fingerprint as lowercase hex:

```text
https=127.0.0.1:9443 certificate_sha256=<64 hex characters>
```

Deliver the public certificate and token to callers through a trusted channel.
Check the certificate fingerprint against that startup record, then add the
certificate as an explicit client trust root. Keep normal TLS hostname/IP SAN
verification enabled. Each new process needs its new trust root. Do not use
`curl -k` or disable certificate verification.

## Existing certificate and token files

To use an existing PEM certificate chain and its matching private key:

```sh
./bin/relais-signal \
  -listen=127.0.0.1:9443 \
  -control=http://127.0.0.1:9200 \
  -cert=frontend.pem -key=frontend-key.pem \
  -advertise-ips=127.0.0.1 \
  -token-file=frontend-token.txt
```

The certificate's validity period is checked at startup. `-advertise-ips` is
optional for loaded certificates; when supplied, each IP must match a SAN.
The leaf fingerprint is printed for loaded certificates too. `-cert-out` is
only for generated certificates.

`-token-env` names the environment variable to read (default:
`RELAIS_SIGNAL_TOKEN`). `-token-file` takes precedence over the environment and
reads a token with an optional trailing newline. Tokens must contain 32 to
16384 bearer-token characters with no internal whitespace. Generate a random token; the minimum length is not an entropy check.
Authentication compares SHA-256 token digests in constant time. Protect token
and key files with restrictive permissions. The frontend has no token-value flag,
never logs the token, and strips the bearer credential before contacting
control. There is one token per frontend process, with access to all calls in
that test; it is not a multi-tenant service.

## API

Every request requires one `Authorization: Bearer <token>` header, including
status, clock and requests for unknown paths. Invalid tokens return 401.

| Method and path | Behavior |
| --- | --- |
| `POST /calls` | Forward the SDP offer to control; return its SDP answer and relative `Location: /calls/{id}`. Use `Content-Type: application/sdp`. Only one `worker` query parameter is allowed; its value must contain 1 to 256 letters, digits, `-` or `_`. |
| `DELETE /calls/{id}` | Hang up through control. IDs must be one segment using letters, digits, `-` or `_`. |
| `GET /status` | Fetch control status and return the sanitized polling view. |
| `GET /clock` | Return frontend-host `received_at` and `sent_at` UTC timestamps in RFC 3339 format. |

With a valid token, unknown paths return 404 and unsupported methods on known
paths return 405. HEAD is refused. There are no move, kill, drain, registration,
metrics, static files or arbitrary proxy routes. Noncanonical paths and encoded
path segments are refused without redirects. Unknown query keys, malformed
queries, duplicate `worker` values and invalid worker names return 400. DELETE,
status and clock accept no query parameters. Outbound queries are rebuilt from
the validated worker name; DELETE sends an empty query. `OPTIONS *` also passes
through authentication and is refused.

Call creation is an SDP exchange, so its answer necessarily advertises the
media address callers must reach. The sanitized status view omits worker
addresses and call owner addresses. It retains call IDs, lease epochs, move
counts, heartbeat and move timestamps, loss counts, and recent takeover/move
events with numeric measurement data. Numeric worker names remain unchanged
for existing caller polling. Other names become `worker-<HMAC>` identifiers
using HMAC-SHA-256 with a random key held only in memory for that frontend process. The same name maps
to the same identifier in calls and events within the run; a new run uses a
new key and different aliases.
Raw error messages and diagnostic IDs are omitted. Failed events retain a
nonempty generic error, `lost`, and the known loss cause. Unknown enum labels
become `unknown`. New private fields do not automatically enter the public view.
The status shape remains decodable as `controlplane.Status`; address fields
are absent. Responses use `Cache-Control: no-store`.

The clock samples bracket handler execution: receive is captured before
authentication, and send immediately before JSON encoding. This endpoint
measures the **frontend host**, not a separately hosted control plane or worker.
It does not prove clock agreement across hosts. A caller must retain its own
send/receive times and judge uncertainty separately. Cross-host mapping
(caller to frontend, and frontend to control) is the responsibility of
[distributed event evidence, #109](https://github.com/relaishq/relais/issues/109).
A deployment either colocates the frontend with control or supplies a measured
inter-host clock bound, for example through chrony or a cloud time service.
A configured time source alone is not proof of the bound.

For example, in a separate shell with the same token and trusted certificate:

```sh
curl --cacert "$run_dir/frontend.pem" \
  -H "Authorization: Bearer $RELAIS_SIGNAL_TOKEN" \
  https://127.0.0.1:9443/clock
```

The existing external caller must supply an HTTP client that trusts this
certificate and attaches the token to signaling and status requests. This
command does not change the caller client or implement distributed event joins.

## Limits and failures

Requests are limited to 64 KiB and fully read before proxying, so an oversized
offer returns 413 without starting a call. Signaling responses are also bounded
to 64 KiB; private status responses are bounded to 4 MiB. Response headers are
allowlisted. Private failure bodies, redirects, absolute call locations and
transport errors never pass through to callers.

The server uses a 5-second header timeout, 15-second read/write timeouts,
a 30-second idle timeout, and a 32 KiB header limit. Upstream operations have
a 10-second deadline, a 3-second dial/TLS handshake timeout, and a 32 KiB
response-header limit. Upstream HTTPS uses normal certificate verification.
TLS 1.2 is the minimum frontend version. Interrupt or SIGTERM stops accepting
connections and allows up to 5 seconds for active requests before closing them.

Signaling mutations are never retried by the frontend. An upstream failure or
lost response can leave the call creation or deletion outcome uncertain.
Inspect status before deciding the next action; a generic error is not proof
that nothing changed. A POST can succeed upstream while frontend response
validation fails, for example when the SDP answer exceeds 64 KiB or the call
location is invalid. The resulting 502 hides the resource location and can
orphan a live call. Reconcile through status and private control before
retrying; the frontend does not automatically delete such calls.

## Local verification

```sh
go vet ./...
go test -race -count=1 ./cmd/relais-signal
make build
```

The tests use trusted HTTPS, an in-process control plane, an actual media worker
and relay, and a Pion caller. They prove call creation, echoed RTP, sanitized
status and hangup. Additional checks cover tokens, refused routes, oversized
requests, upstream failures/cancellation/timeouts, clock timestamps,
certificate generation/loading, trust-root rejection and graceful shutdown.
They start no Redis process.
