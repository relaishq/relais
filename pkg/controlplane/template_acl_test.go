package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Exercise the shipped ACL with the production lease, registration journal,
// rebuild and HTTP readiness paths, rather than an unrestricted test client.
func TestControlTemplateACLMutationReadiness(t *testing.T) {
	if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") != "1" {
		t.Skip("requires an explicitly owned Redis")
	}
	address := redistestenv.Address(t)
	require.NoError(t, redisendpoint.ValidateDev(address))
	data, err := os.ReadFile("../../deploy/aws/preview.yaml")
	require.NoError(t, err)
	var template struct {
		Resources map[string]struct {
			Properties struct {
				AccessString string `yaml:"AccessString"`
			} `yaml:"Properties"`
		} `yaml:"Resources"`
	}
	require.NoError(t, yaml.Unmarshal(data, &template))
	access := template.Resources["ControlStoreUser"].Properties.AccessString
	require.NotEmpty(t, access)
	admin := redis.NewClient(&redis.Options{Addr: address})
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	for _, missingBoundCheck := range []bool{false, true} {
		name := "template"
		if missingBoundCheck {
			name = "missing-record-bound-command"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			token := randomToken()
			prefix := "relais:preview:acl-test:" + token + ":"
			user := "control-test-" + token
			password := randomToken()
			permissions := strings.Fields(access)
			if missingBoundCheck {
				permissions = append(permissions, "-hlen")
			}
			args := []interface{}{"ACL", "SETUSER", user, "reset", ">" + password}
			for _, permission := range permissions {
				args = append(args, permission)
			}
			require.NoError(t, admin.Do(ctx, args...).Err())
			t.Cleanup(func() {
				require.NoError(t, admin.Del(ctx, prefix+"control:{control}").Err())
				require.NoError(t, admin.Do(ctx, "ACL", "DELUSER", user).Err())
			})
			store, err := sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: address, Username: user, Password: password, Prefix: prefix}, make([]byte, 32))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			workerAddress := netip.MustParseAddrPort("127.0.0.1:23456")
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				privateapi.Write(w, mediaworker.WorkerStatus{Address: workerAddress, InventoryKnown: true})
			}))
			t.Cleanup(worker.Close)
			plane := New(&fakeRelay{}, store)
			plane.BeginBootstrap([]string{"worker"})
			plane.SetRelayReady(true)
			require.NoError(t, plane.EnableRestart())
			runContext, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- plane.RunManaged(runContext, []FixedWorker{{Name: "worker", URL: worker.URL}}) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("managed control did not stop")
				}
				require.NoError(t, plane.ReleaseControl(ctx))
			})
			var registry RelayWorkerRegistry
			relay := &registryRecorder{instance: "template-test"}
			status := func() (bool, *RebuildStatus) {
				if plane.controlAuthority() {
					require.NoError(t, registry.Sync(ctx, plane, relay))
				}
				response := httptest.NewRecorder()
				plane.ProcessHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
				var body struct {
					Ready   bool           `json:"ready"`
					Rebuild *RebuildStatus `json:"rebuild"`
				}
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
				return body.Ready, body.Rebuild
			}
			if missingBoundCheck {
				require.EventuallyWithT(t, func(c *assert.CollectT) {
					ready, rebuild := status()
					require.False(c, ready)
					require.NotNil(c, rebuild)
					require.NotEmpty(c, rebuild.Error)
					// Redis versions use different error codes and prose for Lua ACL
					// failures. Check the structured denial from the actual script.
					entries, err := admin.Do(ctx, "ACL", "LOG", 128).Slice()
					require.NoError(c, err)
					denied := false
					for _, entry := range entries {
						fields, ok := entry.([]interface{})
						require.True(c, ok)
						require.Zero(c, len(fields)%2)
						record := make(map[interface{}]interface{}, len(fields)/2)
						for i := 0; i < len(fields); i += 2 {
							record[fields[i]] = fields[i+1]
						}
						if record["username"] == user && record["reason"] == "command" && record["object"] == "hlen" && record["context"] == "lua" {
							denied = true
						}
					}
					require.True(c, denied, "no Lua HLEN denial for this control user; rebuild error: %s", rebuild.Error)
				}, 3*time.Second, 10*time.Millisecond)
				require.ErrorIs(t, plane.End(ctx, "missing"), ErrNotReady)
				return
			}
			require.Eventually(t, func() bool { ready, _ := status(); return ready }, 3*time.Second, 10*time.Millisecond)
			_, rebuild := status()
			require.Equal(t, "ready", rebuild.Phase)
			lease, err := plane.controlLease()
			require.NoError(t, err)
			records, err := store.ControlRecords(ctx, lease)
			require.NoError(t, err)
			require.Contains(t, records, "worker:worker")
			require.NoError(t, plane.MarkDraining("worker"))
		})
	}
}
