package operatorrehearsal

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/host"
)

// This runs inside TestContainerTemplateBoot, on its real relay process and
// private peers. It does not add a second relay to the A/B comparison topology.
func verifyRelayIsolation(t *testing.T, ctx context.Context, d *Deployment) {
	t.Helper()
	run := func(slot, script string) {
		t.Helper()
		args, err := d.execArgs(slot, []string{"-i"}, "python3", "-")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.command(ctx, "relay-isolation-proof", args, []byte(script)); err != nil {
			diagnostic, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer done()
			query, lookupErr := d.execArgs("relay", nil, "nft", "-j", "list", "table", "inet", "relais_gate_isolation")
			if lookupErr == nil {
				result, readErr := d.command(diagnostic, "relay-isolation-json", query, nil)
				t.Logf("NFT_ISOLATION_TABLE_JSON: %s\nstderr: %s\nread error: %v", d.Topology.Redact(string(result.Stdout)), d.Topology.Redact(string(result.Stderr)), readErr)
			} else {
				t.Logf("NFT_ISOLATION_TABLE_JSON unavailable: %v", lookupErr)
			}
			t.Fatal(err)
		}
	}
	process, err := host.Assets.ReadFile("preview_process.py")
	if err != nil {
		t.Fatal(err)
	}
	isolation, err := host.Assets.ReadFile("relay_isolation.py")
	if err != nil {
		t.Fatal(err)
	}
	checker, err := assets.ReadFile("assets/isolation_check.py")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal((&rehearsal{topology: d.Topology}).parameters())
	if err != nil {
		t.Fatal(err)
	}
	prefix := string(process) + "\n" + string(isolation) + "\n" + string(checker) + "\np=json.loads(" + fmt.Sprintf("%q", string(raw)) + ")\n"
	// A real UDP echo peer and management HTTPS-port peer. Both end themselves.
	run("shared", prefix+`
import urllib.parse
address=urllib.parse.urlparse(p['control'])
peer="""import socket,threading,http.server,sys
ip,port=sys.argv[1],int(sys.argv[2])
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind((ip,port))
def echo():
 while True:
  data,addr=s.recvfrom(1024);s.sendto(data,addr)
threading.Thread(target=echo,daemon=True).start()
http.server.HTTPServer((ip,443),http.server.SimpleHTTPRequestHandler).serve_forever()
"""
command(['systemd-run','--quiet','--collect','--unit=relais-isolation-peer','--property=RuntimeMaxSec=90s','/usr/bin/python3','-c',peer,address.hostname,str(address.port)])
`)
	probes := `
import urllib.parse
address=urllib.parse.urlparse(p['control'])
def tcp(host,port):
 with socket.create_connection((host,port),timeout=.5): pass
def udp():
 with socket.socket(socket.AF_INET,socket.SOCK_DGRAM) as s:
  s.settimeout(.5);s.sendto(b'proof',(address.hostname,address.port))
  assert s.recv(1024)==b'proof'
def blocked(f):
 try: f()
 except (OSError,TimeoutError): return
 raise AssertionError('call traffic survived isolation')
def baseline():
 tcp(address.hostname,address.port);tcp(p['store'],p['storePort']);udp()
`
	run("relay", prefix+probes+`
# Wait only for the owned helper's startup, before injecting the fault.
until=time.monotonic()+5
while True:
 try: tcp(address.hostname,443);udp();break
 except OSError:
  if time.monotonic()>until: raise
  time.sleep(.1)
baseline()
import http.client
existing=http.client.HTTPConnection(address.hostname,address.port,timeout=.5)
existing.request('GET','/status');existing.getresponse().read()
assert existing.sock is not None, 'private HTTP did not keep its existing flow'
media=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);media.settimeout(.5)
def existing_udp():
 media.sendto(b'existing-flow',(address.hostname,address.port))
 assert media.recv(1024)==b'existing-flow'
existing_udp()
# Fix only the generated UUID so the readback must match this fault's owner.
from unittest.mock import patch
generation=uuid.uuid4()
with patch.object(uuid,'uuid4',return_value=generation):
 assert execute('relay-isolate',dict(p,isolationSeconds=15))['completed']
rules=command([shutil.which('nft'),'-j','list','table','inet',ISOLATION_TABLE])
check_isolation_json(rules,generation.hex)
blocked(lambda: (existing.request('GET','/status'),existing.getresponse().read()))
blocked(existing_udp)
existing.close();media.close()
blocked(lambda: tcp(address.hostname,address.port))
blocked(lambda: tcp(p['store'],p['storePort']))
blocked(udp)
tcp(address.hostname,443)
with socket.socket() as local:
 local.bind(('127.0.0.1',0));local.listen()
 tcp('127.0.0.1',local.getsockname()[1])
`)
	run("shared", prefix+`
a=urlparse(p['relay'])
try:
 with socket.create_connection((a.hostname,a.port),timeout=.5): pass
except OSError: pass
else: raise AssertionError('inbound private HTTP survived isolation')
`)
	run("relay", prefix+probes+`
assert execute('relay-restore',p)['completed']
assert execute('relay-restore',p)['completed']
baseline()
assert not isolation_table(shutil.which('nft'),command)
# Exit this operator process with the fault still active.
execute('relay-isolate',dict(p,isolationSeconds=2))
blocked(lambda: tcp(address.hostname,address.port))
`)
	run("relay", prefix+probes+`
# A new process observes self-restoration without requesting it.
until=time.monotonic()+6
while isolation_table(shutil.which('nft'),command):
 if time.monotonic()>until: raise AssertionError('expiry did not delete its table')
 time.sleep(.1)
baseline()
execute('relay-restore',p)
`)
	run("shared", prefix+`
a=urlparse(p['relay'])
with socket.create_connection((a.hostname,a.port),timeout=2): pass
`)
	t.Log("VERIFIED: real host isolation blocks UDP, private HTTP and store, keeps management/loopback; restore and process-independent expiry restore traffic")
}
