package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func fixtureConfig() Config {
	return Config{Name: "relais-fixture", Version: "0.3.0", Architecture: "x86_64", ArchiveSHA256: strings.Repeat("9", 64)}
}
func fixtureTopology(t *testing.T) *Topology {
	t.Helper()
	topology, err := Load(os.DirFS("../.."), fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	return topology
}

func TestTemplateTopologyUsesVerifiedBootFiles(t *testing.T) {
	topology := fixtureTopology(t)
	want := map[string]string{"relay": "10.42.0.10", "w1": "10.42.10.11", "w2": "10.42.11.12", "shared": "10.42.10.20"}
	for slot, ip := range want {
		host := topology.Hosts[slot]
		if host.IP != ip {
			t.Fatalf("%s address: %s", slot, host.IP)
		}
		env := host.Metadata.Files["/etc/relais/node.env"].Content
		for _, required := range []string{"RELAIS_REDIS_ADDR=" + StoreEndpoint + ":16379\n", "RELAIS_REDIS_TLS=1\n", "RELAIS_REDIS_CLUSTER=1\n", "RELAY_PUBLIC=10.42.0.10\n", "WORKER_THREE=\n"} {
			if !strings.Contains(env, required) {
				t.Fatalf("%s missing %s", slot, required)
			}
		}
		if !strings.Contains(host.UserData, "resource='"+host.Resource+"'") || strings.Contains(host.UserData, "${AWS::") {
			t.Fatal("UserData not resolved")
		}
		bundle, err := topology.Bundle(slot)
		if err != nil {
			t.Fatal(err)
		}
		for _, other := range Slots {
			if other != slot {
				if _, found := bundle["metadata/"+resources[other]+".json"]; found {
					t.Fatal("other host metadata exposed")
				}
			}
		}
		for id := range topology.Secrets {
			_, found := bundle["secrets/"+id]
			if found != contains(host.Secrets, id) {
				t.Fatal("secret scope differs from role")
			}
		}
	}
	if !reflect.DeepEqual(topology.Hosts["w1"].Secrets, []string{"WorkerPassword", "SnapshotKey"}) {
		t.Fatal("wrong worker role")
	}
	if !reflect.DeepEqual(topology.Hosts["shared"].Secrets, []string{"ControlPassword", "SnapshotKey", "SignalToken"}) {
		t.Fatal("wrong shared role")
	}
	if len(topology.Users) != 3 || len(topology.TemplateSHA256) != 64 {
		t.Fatal("missing store or identity")
	}
	for id, value := range topology.Secrets {
		if len(value) != 64 {
			t.Fatal("secret length")
		}
		for _, ch := range value {
			if id == "SnapshotKey" && !strings.ContainsRune("0123456789abcdef", ch) {
				t.Fatal("snapshot alphabet")
			}
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", ch) {
				t.Fatal("secret alphabet")
			}
		}
	}
}
func contains(values []string, value string) bool {
	for _, s := range values {
		if s == value {
			return true
		}
	}
	return false
}

func copiedTemplate(t *testing.T) fstest.MapFS {
	t.Helper()
	root := os.DirFS("../..")
	result := fstest.MapFS{}
	for _, dir := range []string{"deploy/aws/boot", "deploy/aws/units"} {
		entries, err := fs.ReadDir(root, dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := dir + "/" + entry.Name()
			data, x := fs.ReadFile(root, name)
			if x != nil {
				t.Fatal(x)
			}
			result[name] = &fstest.MapFile{Data: data}
		}
	}
	b, err := fs.ReadFile(root, "deploy/aws/preview.yaml")
	if err != nil {
		t.Fatal(err)
	}
	result["deploy/aws/preview.yaml"] = &fstest.MapFile{Data: b}
	return result
}
func TestTemplateRefusesDriftAndUnsupportedInputs(t *testing.T) {
	for _, row := range []struct{ name, old, new string }{
		{"unit-copy", "Description=Relais preview worker", "Description=changed worker"},
		{"intrinsic", "Store.ConfigurationEndPoint.Address", "Unsupported.Address"},
		{"store-tls", "TransitEncryptionMode: required", "TransitEncryptionMode: preferred"},
		{"store-cluster", "ClusterMode: enabled", "ClusterMode: disabled"},
	} {
		t.Run(row.name, func(t *testing.T) {
			files := copiedTemplate(t)
			entry := files["deploy/aws/preview.yaml"]
			if !bytes.Contains(entry.Data, []byte(row.old)) {
				t.Fatal("mutation did not match")
			}
			entry.Data = bytes.ReplaceAll(entry.Data, []byte(row.old), []byte(row.new))
			if _, err := Load(files, fixtureConfig()); err == nil {
				t.Fatal("accepted invalid template")
			}
		})
	}
	for _, port := range []int{6379, 7000, 7005, 1, 65536} {
		c := fixtureConfig()
		c.StorePort = port
		if _, err := Load(os.DirFS("../.."), c); err == nil {
			t.Fatalf("accepted port %d", port)
		}
	}
	c := fixtureConfig()
	c.ExpectedTemplateSHA256 = strings.Repeat("0", 64)
	if _, err := Load(os.DirFS("../.."), c); err == nil {
		t.Fatal("accepted wrong digest")
	}
	c = fixtureConfig()
	c.Version = "../latest"
	if _, err := Load(os.DirFS("../.."), c); err == nil {
		t.Fatal("accepted unpinned release")
	}
}
func TestACLSelectorsStayWhole(t *testing.T) {
	rules, err := ACLRules("on -@all +ping (+eval\n ~a:* ~b:*) (+hget ~c:*)")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rules, []string{"on", "-@all", "+ping", "(+eval ~a:* ~b:*)", "(+hget ~c:*)"}) {
		t.Fatal(rules)
	}
	for _, s := range []string{"on (+eval", "on ) (", ""} {
		if _, err = ACLRules(s); err == nil {
			t.Fatal("accepted malformed ACL")
		}
	}
	topology := fixtureTopology(t)
	for _, u := range topology.Users {
		if u.Name == "relay" {
			n := 0
			for _, rule := range u.Rules {
				if strings.HasPrefix(rule, "(") {
					n++
				}
			}
			if n != 7 {
				t.Fatal("lost relay selectors")
			}
		}
	}
}
func python(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	binary, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	cmd := exec.Command(binary, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C"}
	return cmd
}

func TestScopedTemplateReadRefusesOtherHostsAndSecrets(t *testing.T) {
	topology := fixtureTopology(t)
	bundle, err := topology.Bundle("w1")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = writeFiles(dir, bundle); err != nil {
		t.Fatal(err)
	}
	// All secret files are present: only the production shim scope can refuse.
	if err = writeFiles(dir, map[string][]byte{"secrets/ControlPassword": []byte("must-not-read"), "metadata/SharedHost.json": []byte("must-not-read")}); err != nil {
		t.Fatal(err)
	}
	metadata := func(resource string) []string {
		return []string{"cloudformation", "describe-stack-resource", "--stack-name", topology.Config.Name, "--logical-resource-id", resource, "--region", localRegion, "--query", "StackResourceDetail.Metadata", "--output", "text"}
	}
	secret := func(name string) []string {
		return []string{"secretsmanager", "get-secret-value", "--secret-id", name, "--region", localRegion, "--query", "SecretString", "--output", "text"}
	}
	for _, row := range []struct {
		args     []string
		accepted bool
	}{{metadata("BootFiles"), true}, {metadata("WorkerOne"), true}, {secret("WorkerPassword"), true}, {metadata("SharedHost"), false}, {secret("ControlPassword"), false}, {[]string{"ssm", "send-command"}, false}, {append(secret("WorkerPassword"), "--secret-id", "WorkerPassword"), false}, {secret("../ControlPassword"), false}} {
		cmd := python(t, append([]string{"-I", filepath.Join(dir, "template-read")}, row.args...)...)
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		err = cmd.Run()
		if row.accepted {
			if err != nil || out.Len() == 0 {
				t.Fatalf("allowed read refused: %v %s", err, stderr.String())
			}
		} else if err == nil || out.Len() != 0 || stderr.String() != "local template read refused\n" {
			t.Fatalf("outside scope was not refused: %v", err)
		}
	}
	if err = os.Remove(filepath.Join(dir, "secrets/WorkerPassword")); err != nil {
		t.Fatal(err)
	}
	cmd := python(t, append([]string{"-I", filepath.Join(dir, "template-read")}, secret("WorkerPassword")...)...)
	if err = cmd.Run(); err == nil {
		t.Fatal("missing credential did not fail")
	}
}

func TestTemplateLoaderMaterializesActualUnitsOffline(t *testing.T) {
	topology := fixtureTopology(t)
	for _, slot := range Slots {
		t.Run(slot, func(t *testing.T) {
			dir := t.TempDir()
			common, _ := json.Marshal(topology.Common)
			host, _ := json.Marshal(topology.Hosts[slot].Metadata)
			if err := writeFiles(dir, map[string][]byte{"etc/relais/common.json": common, "etc/relais/host.json": host}); err != nil {
				t.Fatal(err)
			}
			cmd := python(t, "-I", "../../deploy/aws/boot/load.py", dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("actual loader: %v %s", err, out)
			}
			units, err := topology.Units(slot)
			if err != nil {
				t.Fatal(err)
			}
			for _, unit := range units {
				b, x := os.ReadFile(filepath.Join(dir, "etc/systemd/system", unit.Name))
				if x != nil || string(b) != topology.Common.Files["/etc/systemd/system/"+unit.Name].Content {
					t.Fatal("actual template unit not materialized")
				}
			}
		})
	}
}

func TestCredentialAndClockScriptsUseActualSandbox(t *testing.T) {
	topology := fixtureTopology(t)
	units, err := topology.Units("shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 || units[1].Name != "relais-signal.service" || !reflect.DeepEqual(units[1].Sources, []string{"/etc/relais/node.env", "/run/relais/signal.token"}) {
		t.Fatal(units)
	}
	probe, err := CredentialProbe(units)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"-p User=nobody", "LoadCredential=probe:", "-p 'ProtectSystem=strict'", "-p 'PrivateTmp=yes'"} {
		if !strings.Contains(probe, s) {
			t.Fatal("probe lacks sandbox: " + s)
		}
	}
	if strings.Contains(probe, "cat ") {
		t.Fatal("probe prints credentials")
	}
	script, err := topology.SetupScript("shared")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"chrony-config.service", "systemd-networkd.service", "daemon-reload", "restart chronyd", "mount --make-rshared /", "findmnt -no PROPAGATION /", "len(rows) != 1", "b'-x' not in args"} {
		if !strings.Contains(script, s) {
			t.Fatal("setup lacks " + s)
		}
	}
	if strings.Index(script, "len(rows) != 1") > strings.Index(script, "waitsync") {
		t.Fatal("source precheck runs after synchronization wait")
	}
	if !strings.Contains(chronyDropIn, "chronyd -x") {
		t.Fatal("daemon could change runner clock")
	}
}

func TestCPULayoutUsesAllowedAffinityAndConfigurablePair(t *testing.T) {
	allowed, err := ParseCPUList("2-3,7,11-12")
	if err != nil {
		t.Fatal(err)
	}
	layout, err := Layout(allowed, []int{7, 12})
	if err != nil {
		t.Fatal(err)
	}
	if CPUList(layout.Caller) != "7,12" || CPUList(layout.Services) != "2,3,11" {
		t.Fatal(layout)
	}
	if !reflect.DeepEqual(DriverCommand(layout, "/bin/driver", "arg"), []string{"taskset", "-c", "2,3,11", "/bin/driver", "arg"}) {
		t.Fatal("driver not on services")
	}
	for _, row := range []struct{ allowed, caller []int }{{[]int{0, 1}, nil}, {[]int{0, 1, 2, 2}, nil}, {[]int{2, 3, 7, 8}, []int{0, 1}}, {[]int{2, 3, 7, 8}, []int{2, 2}}} {
		if _, err = Layout(row.allowed, row.caller); err == nil {
			t.Fatal("invalid CPU layout accepted")
		}
	}
	for _, mask := range []string{"1-0", "a", "-1", "1-999999"} {
		if _, err = ParseCPUList(mask); err == nil {
			t.Fatal("invalid mask accepted")
		}
	}
}

func TestRunningClockCheckRejectsWrongSourcesAndMissingNoAdjust(t *testing.T) {
	for _, row := range []struct {
		name, args, sources string
		pass                bool
		reason              string
	}{
		{"reference", "chronyd\x00-x\x00-f\x00/etc/chrony.conf\x00", "^* " + TimeIP + " 1 0 377 0 +0ns[+0ns] +/- 1ms", true, ""},
		{"chronyc-table", "chronyd\x00-x\x00-f\x00/etc/chrony.conf\x00", "MS Name/IP address         Stratum Poll Reach LastRx Last sample\n===============================================================================\n^* " + TimeIP + " 1 0 77 0 +31ns[+162ns] +/- 5811ns", true, ""},
		{"wrong-source", "chronyd\x00-x\x00", "^* 192.0.2.123 1 0 377 0 +0ns", false, "wrong-time-source"},
		{"extra-source", "chronyd\x00-x\x00", "^* " + TimeIP + " 1\n^? 192.0.2.123 1", false, "source count is 2"},
		{"peer-is-a-source", "chronyd\x00-x\x00", "^* " + TimeIP + " 1\n=+ 192.0.2.123 1", false, "source count is 2"},
		{"not-selected", "chronyd\x00-x\x00", "^? " + TimeIP + " 1", false, "reference is not selected"},
		{"clock-adjustment-enabled", "chronyd\x00-f\x00/etc/chrony.conf\x00", "^* " + TimeIP + " 1", false, "daemon is missing -x"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_ = python(t, "--version")
			dir := t.TempDir()
			if err := writeFiles(dir, map[string][]byte{"42/cmdline": []byte(row.args), "systemctl": []byte("#!/bin/sh\nprintf 42\n"), "chronyc": []byte("#!/bin/sh\ncase $2 in sources) printf '%s\\n' " + quote(row.sources) + ";; tracking) printf 'Leap status     : Normal\\n';; *) exit 1;; esac\n")}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"systemctl", "chronyc"} {
				if err := os.Chmod(filepath.Join(dir, name), 0755); err != nil {
					t.Fatal(err)
				}
			}
			// Only the process filesystem path changes. Execute the generated Python
			// guard, including its real NUL-delimited command-line parsing.
			script := strings.ReplaceAll(ClockCheck(), "'/proc/'", strconv.Quote(dir+"/"))
			cmd := exec.Command("/bin/sh", "-eu")
			cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH")}
			cmd.Stdin = strings.NewReader(script)
			output, err := cmd.CombinedOutput()
			if (err == nil) != row.pass || row.reason != "" && !strings.Contains(string(output), row.reason) {
				t.Fatalf("MUTATION: chrony-source-table pass=%v err=%v output=%s", row.pass, err, output)
			}
		})
	}
}
