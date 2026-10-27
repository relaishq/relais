// Package operatorrehearsal runs the public template on disposable local containers.
// It owns resources and transport, never gate decisions or effect retries.
package operatorrehearsal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	StoreEndpoint = "store.rehearsal.test"
	StoreNode     = "store-node.rehearsal.test"
	SignalName    = "signal.rehearsal.test"
	StoreIP       = "10.42.11.50"
	TimeIP        = "10.42.11.60"
	SignalIP      = "10.42.1.40"
	SignalingURL  = "https://" + SignalIP
	localRegion   = "rehearsal-local-0"
)

var Slots = []string{"relay", "w1", "w2", "shared"}
var resources = map[string]string{"relay": "RelayHost", "w1": "WorkerOne", "w2": "WorkerTwo", "shared": "SharedHost"}
var identifier = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

type Config struct {
	Name, Version, Architecture, ArchiveSHA256 string
	// ExpectedTemplateSHA256 optionally binds a separately pinned template.
	ExpectedTemplateSHA256 string
	StorePort              int
}
type File struct {
	Content string `json:"content"`
	Mode    string `json:"mode"`
}
type Metadata struct {
	Files map[string]File `json:"RelaisFiles"`
}
type Host struct {
	Slot, Resource, IP, UserData string
	Metadata                     Metadata
	Secrets                      []string
}
type StoreUser struct {
	Name, Secret string
	Rules        []string
}
type Topology struct {
	Config                                Config
	TemplateSHA256, EngineVersion, Subnet string
	Common                                Metadata
	Hosts                                 map[string]Host
	Users                                 []StoreUser
	Secrets                               map[string]string
}

type object map[string]any

func obj(v any) object {
	if m, ok := v.(object); ok {
		return m
	}
	m, _ := v.(map[string]any)
	return m
}
func list(v any) []any  { a, _ := v.([]any); return a }
func str(v any) string  { s, _ := v.(string); return s }
func prop(r any) object { return obj(obj(r)["Properties"]) }
func ref(v any) string  { return str(obj(v)["Ref"]) }

// Load verifies every boot/unit copy against its source, then resolves only the
// template inputs the local topology supports. Unknown consumed intrinsics fail.
func Load(root fs.FS, c Config) (*Topology, error) {
	if c.StorePort == 0 {
		c.StorePort = 16379
	}
	if !identifier.MatchString(c.Name) || c.StorePort < 1024 || c.StorePort > 55535 || c.StorePort == 6379 || c.StorePort >= 7000 && c.StorePort <= 7005 {
		return nil, errors.New("invalid-rehearsal-config")
	}
	if c.Architecture != "x86_64" && c.Architecture != "arm64" {
		return nil, errors.New("invalid-architecture")
	}
	b, err := fs.ReadFile(root, "deploy/aws/preview.yaml")
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(b)
	t := &Topology{Config: c, TemplateSHA256: hex.EncodeToString(hash[:]), Hosts: map[string]Host{}, Secrets: map[string]string{}}
	if c.ExpectedTemplateSHA256 != "" && c.ExpectedTemplateSHA256 != t.TemplateSHA256 {
		return nil, errors.New("template-digest-mismatch")
	}
	var doc object
	if err = yaml.Unmarshal(b, &doc); err != nil {
		return nil, errors.New("invalid-template")
	}
	r := obj(doc["Resources"])
	parameters := obj(doc["Parameters"])
	for name, value := range map[string]string{"ReleaseVersion": c.Version, "ArchiveSHA256": c.ArchiveSHA256} {
		pattern := str(obj(parameters[name])["AllowedPattern"])
		re, e := regexp.Compile("^(?:" + pattern + ")$")
		if e != nil || pattern == "" || !re.MatchString(value) {
			return nil, errors.New("invalid-release-pin")
		}
	}
	common, err := json.Marshal(obj(r["BootFiles"])["Metadata"])
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(common, &t.Common) != nil || len(t.Common.Files) == 0 {
		return nil, errors.New("missing-boot-files")
	}
	if err = verifyCopies(root, r, t.Common); err != nil {
		return nil, err
	}
	store := prop(r["Store"])
	if store["Engine"] != "valkey" || store["ClusterMode"] != "enabled" || store["TransitEncryptionEnabled"] != true || store["TransitEncryptionMode"] != "required" || store["NumNodeGroups"] != 1 || store["EngineVersion"] != "9.0" {
		return nil, errors.New("unsupported-store-shape")
	}
	t.EngineVersion = str(store["EngineVersion"])
	t.Subnet = str(prop(r["Vpc"])["CidrBlock"])
	if t.Subnet != "10.42.0.0/16" {
		return nil, errors.New("unsupported-template-subnet")
	}
	params := map[string]any{"Profile": "lean", "EnableSessionManager": "true", "WorkerCallCap": "2", "Architecture": c.Architecture, "ReleaseVersion": c.Version, "ArchiveSHA256": c.ArchiveSHA256, "AWS::Region": localRegion, "AWS::StackName": c.Name}
	for name, raw := range r {
		if obj(raw)["Type"] == "AWS::SecretsManager::Secret" {
			s, e := generateSecret(obj(prop(raw)["GenerateSecretString"]))
			if e != nil {
				return nil, e
			}
			t.Secrets[name] = s
			params[name] = name
		}
	}
	ip := func(resource string) string {
		p := prop(r[resource])
		if s := str(p["PrivateIpAddress"]); s != "" {
			return s
		}
		a := list(p["NetworkInterfaces"])
		if len(a) != 1 {
			return ""
		}
		return str(prop(r[ref(obj(a[0])["NetworkInterfaceId"])])["PrivateIpAddress"])
	}
	params["RelayAddress"] = ip("RelayHost")
	e := evaluator{params: params, conditions: obj(doc["Conditions"]), port: c.StorePort}
	seen := map[string]bool{StoreIP: true, TimeIP: true, SignalIP: true}
	for _, slot := range Slots {
		name := resources[slot]
		raw := obj(r[name])
		addr := ip(name)
		a, parseErr := netip.ParseAddr(addr)
		if parseErr != nil || !netip.MustParsePrefix(t.Subnet).Contains(a) || seen[addr] {
			return nil, errors.New("invalid-template-host-address")
		}
		seen[addr] = true
		if condition := str(raw["Condition"]); condition != "" {
			ok, x := e.condition(condition)
			if x != nil || !ok {
				return nil, errors.New("host-not-in-lean-profile")
			}
		}
		metadata, x := e.value(raw["Metadata"])
		if x != nil {
			return nil, x
		}
		mb, x := json.Marshal(metadata)
		if x != nil {
			return nil, x
		}
		h := Host{Slot: slot, Resource: name, IP: addr}
		if json.Unmarshal(mb, &h.Metadata) != nil || len(h.Metadata.Files) != 2 {
			return nil, errors.New("invalid-host-metadata")
		}
		ud, x := e.value(prop(raw)["UserData"])
		if x != nil {
			return nil, x
		}
		h.UserData = str(ud)
		if h.UserData == "" {
			return nil, errors.New("missing-user-data")
		}
		profile := prop(r[ref(prop(raw)["IamInstanceProfile"])])
		roles := list(profile["Roles"])
		if len(roles) != 1 {
			return nil, errors.New("invalid-host-role")
		}
		for _, policy := range list(prop(r[ref(roles[0])])["Policies"]) {
			for _, statement := range list(obj(obj(policy)["PolicyDocument"])["Statement"]) {
				s := obj(statement)
				actions := list(s["Action"])
				if text := str(s["Action"]); text != "" {
					actions = []any{text}
				}
				if !slices.Contains(actions, any("secretsmanager:GetSecretValue")) {
					continue
				}
				if s["Effect"] != "Allow" {
					return nil, errors.New("unsupported-secret-policy")
				}
				for _, secret := range list(s["Resource"]) {
					id := ref(secret)
					if t.Secrets[id] == "" {
						return nil, errors.New("invalid-secret-scope")
					}
					h.Secrets = append(h.Secrets, id)
				}
			}
		}
		if len(h.Secrets) == 0 {
			return nil, errors.New("empty-secret-scope")
		}
		t.Hosts[slot] = h
	}
	// Follow only the store's attached groups; detached resources must not gain
	// credentials locally that the deployed store would reject.
	attached := object{}
	groups := list(store["UserGroupIds"])
	if len(groups) == 0 {
		return nil, errors.New("missing-store-user-groups")
	}
	for _, groupRef := range groups {
		group := obj(r[ref(groupRef)])
		if group["Type"] != "AWS::ElastiCache::UserGroup" || prop(group)["Engine"] != "valkey" {
			return nil, errors.New("unsupported-store-user-group")
		}
		users := list(prop(group)["UserIds"])
		if len(users) == 0 {
			return nil, errors.New("empty-store-user-group")
		}
		for _, userRef := range users {
			name := ref(userRef)
			if name == "" || obj(r[name])["Type"] != "AWS::ElastiCache::User" {
				return nil, errors.New("unsupported-store-user-reference")
			}
			attached[name] = r[name]
		}
	}
	for _, name := range sortedKeys(attached) {
		p := prop(attached[name])
		passwords := list(obj(p["AuthenticationMode"])["Passwords"])
		if len(passwords) != 1 || p["Engine"] != "valkey" || obj(p["AuthenticationMode"])["Type"] != "password" {
			return nil, errors.New("unsupported-store-user")
		}
		match := regexp.MustCompile(`^\{\{resolve:secretsmanager:\$\{(\w+)\}:SecretString\}\}$`).FindStringSubmatch(str(obj(passwords[0])["Fn::Sub"]))
		if len(match) != 2 || t.Secrets[match[1]] == "" {
			return nil, errors.New("invalid-store-secret")
		}
		rules, x := ACLRules(str(p["AccessString"]))
		if x != nil {
			return nil, x
		}
		t.Users = append(t.Users, StoreUser{Name: str(p["UserName"]), Secret: match[1], Rules: rules})
	}
	return t, nil
}

func verifyCopies(root fs.FS, resources object, common Metadata) error {
	expected := map[string]File{}
	for _, dir := range []string{"boot", "units"} {
		entries, err := fs.ReadDir(root, "deploy/aws/"+dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Name() == "user-data.sh" {
				continue
			}
			b, x := fs.ReadFile(root, "deploy/aws/"+dir+"/"+entry.Name())
			if x != nil {
				return x
			}
			name, mode := "/opt/relais/boot/"+entry.Name(), "000755"
			if dir == "units" {
				name, mode = "/etc/systemd/system/"+entry.Name(), "000644"
			}
			if strings.HasSuffix(entry.Name(), ".conf") {
				name, mode = "/etc/sysctl.d/60-relais-udp.conf", "000644"
			}
			expected[name] = File{string(b), mode}
		}
	}
	if len(expected) != len(common.Files) {
		return errors.New("template-boot-parity")
	}
	for name, f := range expected {
		if common.Files[name] != f {
			return errors.New("template-boot-parity:" + path.Base(name))
		}
	}
	ud, err := fs.ReadFile(root, "deploy/aws/boot/user-data.sh")
	if err != nil {
		return err
	}
	for name, raw := range resources {
		if obj(raw)["Type"] != "AWS::EC2::Instance" {
			continue
		}
		if str(obj(obj(prop(raw)["UserData"])["Fn::Base64"])["Fn::Sub"]) != strings.ReplaceAll(string(ud), "NODE_RESOURCE", name) {
			return errors.New("template-user-data-parity")
		}
	}
	return nil
}

type evaluator struct {
	params     map[string]any
	conditions object
	port       int
}

var substitution = regexp.MustCompile(`\$\{([^}]+)\}`)

func (e evaluator) condition(name string) (bool, error) {
	a := list(obj(e.conditions[name])["Fn::Equals"])
	if len(a) != 2 {
		return false, errors.New("unsupported-condition")
	}
	x, err := e.value(a[0])
	if err != nil {
		return false, err
	}
	y, err := e.value(a[1])
	if err != nil {
		return false, err
	}
	xs, xok := x.(string)
	ys, yok := y.(string)
	if !xok || !yok {
		return false, errors.New("unsupported-condition-values")
	}
	return xs == ys, nil
}
func (e evaluator) value(v any) (any, error) {
	if a, ok := v.([]any); ok {
		out := make([]any, len(a))
		for i, x := range a {
			y, err := e.value(x)
			if err != nil {
				return nil, err
			}
			out[i] = y
		}
		return out, nil
	}
	m := obj(v)
	if m == nil {
		return v, nil
	}
	if len(m) == 1 {
		for fn, arg := range m {
			switch fn {
			case "Ref":
				if x, found := e.params[str(arg)]; found {
					return x, nil
				}
				return nil, errors.New("unsupported-template-reference")
			case "Fn::Base64":
				return e.value(arg) // UserData is executed as text, not encoded again.
			case "Fn::GetAtt":
				a := list(arg)
				if len(a) != 2 {
					return nil, errors.New("unsupported-template-attribute")
				}
				return e.attribute(str(a[0]) + "." + str(a[1]))
			case "Fn::If":
				a := list(arg)
				if len(a) != 3 {
					return nil, errors.New("invalid-template-if")
				}
				yes, err := e.condition(str(a[0]))
				if err != nil {
					return nil, err
				}
				if yes {
					return e.value(a[1])
				}
				return e.value(a[2])
			case "Fn::Sub":
				text, vars := str(arg), object{}
				if a := list(arg); a != nil {
					if len(a) != 2 {
						return nil, errors.New("invalid-template-sub")
					}
					text = str(a[0])
					vars = obj(a[1])
				}
				var failure error
				out := substitution.ReplaceAllStringFunc(text, func(token string) string {
					name := token[2 : len(token)-1]
					if strings.HasPrefix(name, "!") {
						return "${" + name[1:] + "}"
					}
					var x any
					var err error
					if raw, exists := vars[name]; exists {
						x, err = e.value(raw)
					} else if strings.Contains(name, ".") {
						x, err = e.attribute(name)
					} else {
						x, err = e.value(map[string]any{"Ref": name})
					}
					if err != nil {
						failure = err
						return ""
					}
					return fmt.Sprint(x)
				})
				return out, failure
			}
			if strings.HasPrefix(fn, "Fn::") {
				return nil, errors.New("unsupported-template-intrinsic")
			}
		}
	}
	out := map[string]any{}
	for k, x := range m {
		y, err := e.value(x)
		if err != nil {
			return nil, err
		}
		out[k] = y
	}
	return out, nil
}
func (e evaluator) attribute(name string) (any, error) {
	switch name {
	case "Store.ConfigurationEndPoint.Address":
		return StoreEndpoint, nil
	case "Store.ConfigurationEndPoint.Port":
		return strconv.Itoa(e.port), nil
	}
	return nil, errors.New("unsupported-template-attribute")
}
func sortedKeys(m object) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func generateSecret(spec object) (string, error) {
	// Only the template's generated alphanumeric/hex secrets are supported.
	n, ok := spec["PasswordLength"].(int)
	if !ok || n != 64 || spec["ExcludePunctuation"] != true || spec["IncludeSpace"] != false || spec["RequireEachIncludedType"] != false {
		return "", errors.New("unsupported-secret-generation")
	}
	chars := "abcdefghijklmnopqrstuvwxyz0123456789"
	if spec["ExcludeUppercase"] != true {
		chars += "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	}
	chars = strings.Map(func(r rune) rune {
		if strings.ContainsRune(str(spec["ExcludeCharacters"]), r) {
			return -1
		}
		return r
	}, chars)
	if chars == "" {
		return "", errors.New("empty-secret-alphabet")
	}
	out := make([]byte, n)
	for i := range out {
		x, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return "", err
		}
		out[i] = chars[x.Int64()]
	}
	return string(out), nil
}

// ACLRules preserves each parenthesized Valkey selector as one CLI argument.
func ACLRules(access string) ([]string, error) {
	var rules []string
	depth := 0
	var current strings.Builder
	for _, ch := range access {
		if ch == '(' {
			depth++
		}
		if ch == ')' {
			depth--
			if depth < 0 {
				return nil, errors.New("unbalanced-store-acl")
			}
		}
		if ch == ' ' || ch == '\n' || ch == '\t' {
			if depth == 0 {
				if current.Len() > 0 {
					rules = append(rules, strings.Join(strings.Fields(current.String()), " "))
					current.Reset()
				}
				continue
			}
			ch = ' '
		}
		current.WriteRune(ch)
	}
	if depth != 0 {
		return nil, errors.New("unbalanced-store-acl")
	}
	if current.Len() > 0 {
		rules = append(rules, strings.Join(strings.Fields(current.String()), " "))
	}
	if len(rules) == 0 {
		return nil, errors.New("empty-store-acl")
	}
	return rules, nil
}
