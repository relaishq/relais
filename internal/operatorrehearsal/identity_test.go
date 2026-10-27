package operatorrehearsal

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relaishq/relais/pkg/operator"
)

// Docker resolves names before abbreviated IDs. Full 64-hex IDs resolve the
// exact object, even when an unrelated object's name equals its short ID.
type nameCollisionRunner struct {
	owned, foreign string
	names          map[string]string
	selected       string
	commands       []Command
	removed        map[string]bool
}

func (r *nameCollisionRunner) Run(_ context.Context, c Command) CommandResult {
	r.commands = append(r.commands, c)
	switch c.Label {
	case "cleanup-list":
		id := r.owned
		if r.selected != "" {
			id = r.selected
		} else if !contains(c.Args, "--no-trunc") {
			id = id[:12]
		}
		return CommandResult{Code: code(0), Stdout: []byte(id + "\n")}
	case "cleanup-container":
		reference := c.Args[len(c.Args)-1]
		id := reference
		if len(reference) != 64 {
			if named, ok := r.names[reference]; ok {
				id = named
			} else if strings.HasPrefix(r.owned, reference) {
				id = r.owned
			}
		}
		r.removed[id] = true
	}
	return CommandResult{Code: code(0)}
}
func identityDeployment(t *testing.T, runner Runner) *Deployment {
	t.Helper()
	layout, err := Layout([]int{2, 3, 8, 9}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	d, err := NewDeployment(fixtureTopology(t), filepath.Join(dir, "work"), filepath.Join(dir, "release.tar.gz"), layout, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func TestCleanupPreservesContainerNamedAfterOwnedShortID(t *testing.T) {
	owned, foreign := strings.Repeat("a", 64), strings.Repeat("b", 64)
	runner := &nameCollisionRunner{owned: owned, foreign: foreign, names: map[string]string{owned[:12]: foreign}, removed: map[string]bool{}}
	d := identityDeployment(t, runner)
	if err := d.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !runner.removed[owned] || runner.removed[foreign] {
		t.Fatal("cleanup resolved the owned short ID to an unrelated container name")
	}
}
func TestCleanupRejectsNonFullContainerIDs(t *testing.T) {
	for _, id := range []string{strings.Repeat("a", 12), strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64)} {
		runner := &nameCollisionRunner{owned: strings.Repeat("a", 64), selected: id, removed: map[string]bool{}}
		d := identityDeployment(t, runner)
		if err := d.Close(context.Background()); err == nil {
			t.Fatal("cleanup accepted a non-full container ID")
		}
		if len(runner.removed) != 0 {
			t.Fatal("cleanup submitted a deletion for a non-full ID")
		}
	}
}
func TestCleanupWithoutOwnershipSubmitsNothing(t *testing.T) {
	for _, networkID := range []string{"", strings.Repeat("b", 64)} {
		runner := &fakeRunner{result: CommandResult{Code: code(0), Stdout: []byte(strings.Repeat("a", 64))}}
		d := &Deployment{Runner: runner, networkID: networkID}
		if err := d.Close(context.Background()); err == nil {
			t.Fatal("uninitialized cleanup retained deletion authority")
		}
		if len(runner.commands) != 0 {
			t.Fatal("cleanup without ownership issued a Docker command")
		}
	}
}

func fixtureContainerIDs() map[string]string {
	ids := map[string]string{}
	for i, slot := range []string{"relay", "w1", "w2", "shared", "store", "time", "signal-front"} {
		ids[slot] = strings.Repeat(string(rune('1'+i)), 64)
	}
	return ids
}

func TestAdapterRefusesUnconfirmedContainerBinding(t *testing.T) {
	for _, id := range []string{"", strings.Repeat("a", 12), strings.Repeat("a", 63), "relais-fixture-w1"} {
		runner := &fakeRunner{result: CommandResult{Code: code(0)}}
		a := &Adapter{Topology: fixtureTopology(t), Containers: map[string]string{"w1": id}, Runner: runner}
		reply := a.Execute(context.Background(), request())
		if !reply.NotSubmitted || reply.RefusalCode != "invalid-target" || len(runner.commands) != 0 {
			t.Fatal("adapter resolved an unconfirmed container name or prefix")
		}
	}
}
func execTarget(args []string) string {
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "-i":
			continue
		case "-e":
			i++
			continue
		default:
			return args[i]
		}
	}
	return ""
}
func TestDockerOperationsUseConfirmedCreationIDs(t *testing.T) {
	d, runner := bootedFixture(t)
	if err := d.NegativeControls(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Topology: d.Topology, Containers: d.ContainerIDs(), Runner: &serialBootRunner{runner: runner}}
	_ = a.Execute(context.Background(), request())
	ids := d.ContainerIDs()
	known := map[string]bool{}
	for _, id := range ids {
		known[id] = true
	}
	for _, c := range runner.commands {
		if len(c.Args) < 2 {
			continue
		}
		if c.Args[1] == "exec" {
			target := execTarget(c.Args)
			if !dockerID.MatchString(target) || !known[target] {
				t.Fatal("docker exec used a name, prefix or unconfirmed container ID")
			}
		}
		if c.Args[1] == "run" {
			for i, arg := range c.Args {
				if arg == "--network" && (i+1 == len(c.Args) || c.Args[i+1] != d.networkID) {
					t.Fatal("docker run resolved the network by name")
				}
			}
		}
	}
	// Adapter execution must retain the confirmed ID after a container is renamed;
	// the original generated name is not a fallback resource selector.
	req := request()
	req.ID = "2"
	reply := a.Execute(context.Background(), req)
	if reply.Transport != operator.Succeeded || execTarget(runner.commands[len(runner.commands)-1].Args) != ids[req.Target] {
		t.Fatal("adapter lost its creation binding")
	}
	delete(ids, "w1")
	if d.ContainerIDs()["w1"] == "" {
		t.Fatal("caller could erase deployment identity through a returned map")
	}
}

func TestContainerCreationPrefixNeverBecomesExecutionAuthority(t *testing.T) {
	runner := &fakeRunner{result: CommandResult{Code: code(0), Stdout: []byte(strings.Repeat("a", 12))}}
	d := identityDeployment(t, runner)
	if err := d.startContainer(context.Background(), "w1", "host-w1", []string{"docker", "run", "-d", "fixture-image"}); err == nil {
		t.Fatal("container creation accepted an abbreviated ID")
	}
	if _, err := d.execArgs("w1", nil, "true"); err == nil {
		t.Fatal("unconfirmed creation fell back to execution by name")
	}
	if len(runner.commands) != 1 || len(d.ContainerIDs()) != 0 {
		t.Fatal("unconfirmed container creation gained an execution binding")
	}
}

type commandRunner func(context.Context, Command) CommandResult

func (f commandRunner) Run(ctx context.Context, c Command) CommandResult { return f(ctx, c) }
func TestNetworkCreationPrefixStopsBeforeContainerCreation(t *testing.T) {
	original, _ := bootedFixture(t)
	var commands []Command
	base := &bootRunner{}
	runner := commandRunner(func(ctx context.Context, c Command) CommandResult {
		commands = append(commands, c)
		if c.Label == "network" {
			return CommandResult{Code: code(0), Stdout: []byte(strings.Repeat("a", 12))}
		}
		return base.Run(ctx, c)
	})
	d, err := NewDeployment(original.Topology, filepath.Join(t.TempDir(), "work"), original.Archive, original.Layout, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Boot(context.Background()); err == nil {
		t.Fatal("network creation accepted an abbreviated ID")
	}
	for _, c := range commands {
		if len(c.Args) > 1 && (c.Args[1] == "run" || c.Args[1] == "exec") {
			t.Fatal("setup used an unconfirmed network")
		}
	}
	if d.networkID != "" {
		t.Fatal("abbreviated network ID retained cleanup authority")
	}
}
