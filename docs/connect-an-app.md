# Connect an app

Use `relais-signal` in front of the private control plane. Your app's backend
holds the bearer token and forwards the caller's session description (SDP).
The caller sends media directly to the relay's advertised UDP address.
If you started the topology with `start.sh` (see [install](install.md)) or
followed the [deployment guide](deploy.md), signal is already running. Use its
URL, certificate, and existing token, then skip to [Place a call](#place-a-call).
Do not start another frontend or generate a replacement token for that setup.
For your own topology without signal, see [Start signal yourself](#start-signal-yourself).

## Place a call

For `start.sh`, use the printed **Signal URL**, **CA certificate**, and **Token
file** values. Its default state directory is `.relais-local` beside the script,
with mode 0700; `-state DIR` or `STATE_DIR` selects another directory. For a
checkout using the defaults, set these in your backend shell:

```sh
app_signal_url=https://127.0.0.1:34743
app_signal_cert=deploy/local/.relais-local/secrets/signal.pem
app_signal_token_file=deploy/local/.relais-local/secrets/signal.token
```

In an extracted archive, use `.relais-local/secrets/signal.pem` and
`.relais-local/secrets/signal.token`. If you changed the ports or state directory,
use the printed values instead. The script keeps the token across restarts;
it generates a new certificate on each full topology start.

If you followed the manual deployment recipe instead, keep its existing token
and certificate. In that recipe's shell, use:

```sh
app_signal_url=https://127.0.0.1:34743
app_signal_cert="$run_dir/frontend.pem"
app_signal_token_file="$run_dir/app-signal.token"
(umask 077; printf '%s\n' "$RELAIS_SIGNAL_TOKEN" > "$app_signal_token_file")
```

This copies the running setup's token; it does not replace it. Trust the existing
certificate only after comparing its fingerprint over a trusted channel. Keep
normal certificate and IP verification enabled.

Create an `RTCPeerConnection` in your caller with `bundlePolicy: 'max-bundle'`
and `rtcpMuxPolicy: 'require'`. Add `sendrecv` audio and/or video tracks or
transceivers. Create and set the local offer, wait for ICE gathering to finish,
then send the complete `pc.localDescription.sdp` through your backend.
ICE is WebRTC's network-path setup. There is no trickle ICE or PATCH endpoint.
The API uses a WHIP-style exchange: one HTTP offer and answer, rather than a
full claim of WebRTC-HTTP Ingestion Protocol (WHIP) compliance.

Given that complete offer in `offer.sdp`, your backend can send:

```sh
app_call_dir="$(mktemp -d)"
curl --fail-with-body --cacert "$app_signal_cert" \
  -D "$app_call_dir/call.headers" -o "$app_call_dir/answer.sdp" \
  -H "Authorization: Bearer $(cat "$app_signal_token_file")" \
  -H 'Content-Type: application/sdp' \
  --data-binary @offer.sdp \
  "$app_signal_url/calls"
```

Success returns **201 Created**, `Content-Type: application/sdp`, the SDP answer
as the body, and a relative `Location: /calls/{id}`. Retain that location in your
backend, return the answer to the caller, and call
`pc.setRemoteDescription({type: 'answer', sdp: answer})` there.
The offer and answer are limited to 64 KiB. Oversized requests return 413.

Accepted media is Opus audio and/or VP8 video with `sendrecv`, BUNDLE (tracks
share one transport), and `rtcp-mux` (media and control share a port). Audio-only
calls work. Data channels are not supported. There is no agent choice per call.
An optional single `?worker=name` pins placement; its name must be 1–256 letters,
digits, `-`, or `_`. Normally let control choose a worker.

## End the call

Use the exact relative location returned by creation. In the same backend shell:

```sh
call_path="$(awk 'tolower($1) == "location:" {gsub("\r", "", $2); print $2}' "$app_call_dir/call.headers")"
curl --fail-with-body --cacert "$app_signal_cert" \
  -H "Authorization: Bearer $(cat "$app_signal_token_file")" \
  -X DELETE "$app_signal_url$call_path"
```

Successful deletion returns **200 OK** with an empty body. Close the caller's
peer connection as well. Validate that the saved location has the expected
`/calls/{id}` shape before using it in application code.

## Backend and failure rules

Every route requires exactly one `Authorization: Bearer <token>` header;
invalid authentication returns 401, including for status or unknown paths.
There is **no CORS** (cross-origin browser access), and OPTIONS is refused.
Call this API from your backend; do not put the shared token in browser code.
Your backend must authorize each caller and limit which call IDs it can delete.
The browser demo has its own launch checks and is not this API.

`GET /status` provides sanitized control status without private addresses or
snapshot keys. `GET /clock` returns frontend-host `received_at` and `sent_at`
timestamps. It does not establish clock agreement with a separate worker host.
Move, drain, kill, metrics, and registration routes are not exposed by signal.
Unknown paths return 404; unsupported methods return 405. Unknown query keys,
duplicate placement values, and noncanonical or encoded call paths are refused.

Signal never retries mutations. A timeout, lost response, or generic upstream
error can leave creation or deletion uncertain. Check status and reconcile with
private control before retrying. A failed response is not proof that no call
was created. See [the full frontend contract](dev/signal-frontend.md) for limits,
certificate handling, sanitization, and failure cases, and
[the API reference](api_reference.md) for lower-level test surfaces.

## Start signal yourself

Only use this section for your own topology with control at `127.0.0.1:34710`
and no signaling frontend already running. From the repository root:

```sh
signal_manual_dir="$(mktemp -d)"
(umask 077; openssl rand -hex 32 > "$signal_manual_dir/token")
./bin/relais-signal \
  -listen=127.0.0.1:9443 \
  -control=http://127.0.0.1:34710 \
  -advertise-ips=127.0.0.1 \
  -token-file="$signal_manual_dir/token" \
  -cert-out="$signal_manual_dir/frontend.pem"
```

The command stays in the foreground. In your backend shell, use
`https://127.0.0.1:9443` as `app_signal_url`, the generated `frontend.pem` as
`app_signal_cert`, and the generated `token` file as `app_signal_token_file`.
Then return to [Place a call](#place-a-call). The generated self-signed certificate
lasts 24 hours. Compare its printed SHA-256 fingerprint over a trusted channel,
then trust the public PEM explicitly in your client. Keep certificate and IP
verification enabled. Each restart generates a new certificate and trust root.
Do not disable TLS verification.

For an existing certificate, replace `-cert-out` with
`-cert=frontend.pem -key=frontend-key.pem`. Supply both files. `-advertise-ips`
is required for generated certificates; with loaded certificates it is optional,
and every supplied IP must match a certificate Subject Alternative Name (SAN).
`-cert-out` refuses an existing path and cannot be combined with `-cert`.

`-token-file=frontend-token.txt` takes precedence over the environment.
`-token-env` changes the environment variable name (default `RELAIS_SIGNAL_TOKEN`).
Tokens must contain 32–16384 bearer-token characters, without internal whitespace.
Use random tokens and protect token and private-key files. There is one static
token per process, covering all calls; there are no per-call or tenant tokens.
Signal strips the token before forwarding requests to control.

`-listen` requires a literal private or loopback IP and a nonzero port. `-control`
requires an HTTP(S) origin at a literal private or loopback IP, without credentials,
a path other than `/`, a query, or a fragment. The frontend does not use environment
HTTP proxies or follow upstream redirects. For another machine, use its private
IP and the IP SANs callers will reach. Public exposure needs a separately designed
network and authorization boundary; this frontend is for controlled tests.
