package operatorcaller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

func TestBridgeCapsCleanupToManifest(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		t.Run(map[bool]string{false: "later-runtime", true: "earlier-runtime"}[earlier], func(t *testing.T) {
			root := t.TempDir()
			now := time.Now()
			// Exercise the production lifecycle after validation, with startup
			// budget already spent. Validation's media reserve is still required
			// at admission; it does not renew the accepted absolute deadline.
			admitted := now.Add(-3 * time.Minute)
			m := fixtureManifest()
			m.ArmedAt = admitted.Add(-20 * time.Second).UnixMilli()
			m.CreatedAt = m.ArmedAt
			m.Deadline = now.Add(8 * time.Second).UnixMilli()
			m.Plan = NewPlan(m.Phase, 1, m.PublicSHA)
			m.Endpoints.Signaling = "https://127.0.0.1:1/calls"
			if err := m.Validate(m.Identity, admitted); err != nil {
				t.Fatal(err)
			}
			runtimeEnd := now.Add(time.Minute)
			if earlier {
				runtimeEnd = now.Add(6 * time.Second).Truncate(time.Millisecond)
			}
			cutoff := minTime(runtimeEnd, time.UnixMilli(m.Deadline))
			ctx, cancel := context.WithDeadline(context.Background(), runtimeEnd)
			defer cancel()
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
			certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			ca := filepath.Join(root, "ca.pem")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0600); err != nil {
				t.Fatal(err)
			}
			load := filepath.Join(root, "load")
			// A real child writes its final report, then a malformed stream forces
			// the bridge's actual stop and final-publication paths to execute.
			child := "#!/bin/sh\nfor arg do case $arg in -output=*) printf 'report' > \"${arg#-output=}\";; esac; done\nprintf 'not-json\\n'\n"
			if err := os.WriteFile(load, []byte(child), 0700); err != nil {
				t.Fatal(err)
			}
			peer := &mailboxPeer{reply: func(f operator.Frame) (operator.Frame, error) {
				var args MailboxArgs
				if err := json.Unmarshal(f.Request.Args, &args); err != nil {
					return operator.Frame{}, err
				}
				found := false
				value := MailboxValue{Found: &found}
				if f.Request.Operation == operator.MailboxCreate {
					value = MailboxValue{State: Committed}
				} else if args.Key == m.Root()+"/caller-token.json" {
					found = true
					value.Data, _ = json.Marshal(map[string]string{"bearerToken": strings.Repeat("t", 64)})
				} else if args.Key == sequenceKey(m.Root(), -1) {
					found = true
					value.Data, _ = json.Marshal(Feed{1, m.RunID, -1, "caller-begin", Payload{Kind: "begin", Deadline: time.UnixMilli(m.Deadline)}})
				}
				raw, _ := json.Marshal(value)
				zero := 0
				return operator.Frame{Type: "result", Session: f.Session, Result: &operator.Reply{ID: f.Request.ID, Transport: operator.Succeeded, ExitCode: &zero, Payload: raw}}, nil
			}}
			box := &ProtocolMailbox{Peer: peer, Session: m.CallerID, End: runtimeEnd}
			c := BridgeConfig{Identity: m.Identity, Bind: "192.0.2.1:0", CertificateFile: ca}
			if err := runBridge(ctx, c, box, load, fixtureHost(admitted), m); err == nil {
				t.Fatal("invalid child stream was accepted")
			}
			// Serialize with the watcher, which may still be completing its last
			// read as the lifecycle returns. No fake publication logic is used.
			box.mu.Lock()
			requests := append([]operator.Frame(nil), peer.requests...)
			box.mu.Unlock()
			seen := map[string]bool{}
			for _, f := range requests {
				var args MailboxArgs
				if err := json.Unmarshal(f.Request.Args, &args); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"stop.json", "caller.jsonl", "finished.json"} {
					if args.Key == m.Job()+"/"+name && f.Request.Operation == operator.MailboxCreate {
						seen[name] = true
						end, err := strconv.ParseInt(f.Request.DeadlineUnixMS, 10, 64)
						if err != nil || end != cutoff.UnixMilli() || f.Request.RemainingMS > cutoff.Sub(now).Milliseconds() {
							t.Error("MUTATION: caller-cleanup-deadline", name, f.Request, cutoff)
						}
					}
				}
			}
			for _, name := range []string{"stop.json", "caller.jsonl", "finished.json"} {
				if !seen[name] {
					t.Error("real bridge publication path was not exercised", name)
				}
			}
		})
	}
}
