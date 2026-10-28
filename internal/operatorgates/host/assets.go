// Package host embeds the fixed operator procedures. The adapter delivers these
// scripts once; classification and sequencing remain in the Go session.
package host

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
)

//go:embed preview_process.py preview_inventory.py preview_telemetry.py clock.py
var Assets embed.FS

func source(name string) string {
	data, err := Assets.ReadFile(name)
	if err != nil {
		panic(err)
	} // Static embedded asset; not an adapter input.
	return string(data)
}

func encoded(value any) (string, error) {
	data, err := json.Marshal(value)
	return base64.StdEncoding.EncodeToString(data), err
}

// Process renders the original fixed-step wrapper with JSON data, never Python
// expressions supplied by an adapter.
type Stage struct{ Version, Arch, ArchiveSHA256, ManifestSHA256, Pool, Operation string }

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var poolPattern = regexp.MustCompile(`^[A-Za-z0-9+/=]+$`)
var operations = []string{"telemetry", "signal-stop", "signal-start", "move", "status", "store-status", "worker-status", "relay-status", "relay-inspect", "worker-empty", "freeze", "unfreeze", "drain", "worker-stop", "replace", "worker-start", "control-stop", "control-start", "worker-down", "relay-stage", "relay-stop", "relay-replenish"}

func IsRead(op string) bool { return IsPureRead(op) || op == "telemetry" || op == "status" }
func IsPureRead(op string) bool {
	return slices.Contains([]string{"store-status", "worker-status", "relay-status", "relay-inspect", "worker-empty"}, op)
}

func Process(operation string, parameters any, stage *Stage) (string, error) {
	if !slices.Contains(operations, operation) || !IsRead(operation) && stage == nil {
		return "", fmt.Errorf("invalid fixed process step")
	}
	data, err := encoded(parameters)
	if err != nil {
		return "", err
	}
	op, _ := json.Marshal(operation)
	preflight := ""
	if stage != nil {
		if !versionPattern.MatchString(stage.Version) || stage.Arch != "arm64" && stage.Arch != "amd64" && stage.Arch != "x86_64" || !digestPattern.MatchString(stage.ArchiveSHA256) || !digestPattern.MatchString(stage.ManifestSHA256) || !poolPattern.MatchString(stage.Pool) || stage.Operation != "upgrade" && stage.Operation != "rollback" {
			return "", fmt.Errorf("invalid stage proof")
		}
		downloadArch := stage.Arch
		if downloadArch == "amd64" {
			downloadArch = "x86_64"
		}
		preflight = fmt.Sprintf(`
    target=pathlib.Path('/opt/relais/preview/%s')
    receipt=target/'archive.sha256'
    if not receipt.exists():
        command(['env','-u','RELAIS_TEST_ARCHIVE_URL','/opt/relais/boot/download.sh','%s','%s','%s',str(target),'worker relay control signal'])
        receipt.write_text('%s')
    if receipt.read_text().strip()!='%s': raise ValueError('staged archive differs')
    import hashlib
    if hashlib.sha256((target/'compatibility.json').read_bytes()).hexdigest()!='%s': raise ValueError('staged manifest differs')
    with tempfile.TemporaryDirectory(prefix='relais-compat-') as scratch:
        running=pathlib.Path(scratch)/'pool.json'
        running.write_bytes(gzip.decompress(base64.b64decode('%s')))
        checked=subprocess.run([str(target/'relais-compat'),'preflight','--running',str(running),'--target',str(target/'compatibility.json'),'--operation','%s'],capture_output=True,text=True,timeout=15)
        verdict=json.loads(checked.stdout)
        if verdict.get('allowed') is False:
            emit({'kind':'refused'});sys.exit(0)
        if checked.returncode!=0 or verdict.get('allowed') is not True: raise ValueError('unknown compatibility result')
`, stage.Version, stage.Version, downloadArch, stage.ArchiveSHA256, stage.ArchiveSHA256, stage.ArchiveSHA256, stage.ManifestSHA256, stage.Pool, stage.Operation)
	}
	failure := "except Exception:\n    emit({'kind':'unknown'})"
	if IsPureRead(operation) {
		failure = "except Exception as error:\n    emit({'kind':'failed','code':read_failure(error)})"
	}
	return "python3 - <<'PY'\n" + source("preview_process.py") + "\n" + source("preview_telemetry.py") + fmt.Sprintf(`
import base64,gzip,tempfile,sys,datetime
def emit(x): print(base64.b64encode(gzip.compress(json.dumps(x,separators=(',',':')).encode())).decode())
try:
%s
    p=json.loads(base64.b64decode('%s'))
    started=datetime.datetime.now(datetime.timezone.utc).isoformat()
    data=execute(%s,p)
    emit({'kind':'ok','started':started,'ended':datetime.datetime.now(datetime.timezone.utc).isoformat(),'data':data})
%s
PY
`, preflight, data, op, failure), nil
}

func Inventory(slot string, pins any, requireRunning bool, architecture ...string) (string, error) {
	arch := "arm64"
	if len(architecture) > 0 {
		arch = architecture[0]
	}
	if arch != "arm64" && arch != "x86_64" {
		return "", fmt.Errorf("invalid inventory architecture")
	}
	data, err := encoded(pins)
	if err != nil {
		return "", err
	}
	name, _ := json.Marshal(slot)
	running := "False"
	if requireRunning {
		running = "True"
	}
	return "python3 - <<'PY'\n" + source("preview_inventory.py") + fmt.Sprintf(`
import base64,gzip,sys
try:
 result=collect(%s,json.loads(base64.b64decode('%s')),require_running=%s,architecture=%q)
 print(base64.b64encode(gzip.compress(json.dumps(result,separators=(',',':')).encode())).decode())
except Exception as error:
 print('INVENTORY_UNKNOWN:'+failure_category(error))
 sys.exit(3)
PY
`, name, data, running, arch), nil
}

func Clock() string {
	return "python3 - <<'PY'\n" + source("clock.py") + `
import json,gzip,base64
try:
 print(base64.b64encode(gzip.compress(json.dumps(clock_evidence()).encode())).decode())
except Exception:
 print('CLOCK_UNKNOWN')
PY
`
}
