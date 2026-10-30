package agentbridge

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	"golang.org/x/net/websocket"
)

func dialBot(ctx context.Context, endpoint string) (*websocket.Conn, error) {
	cfg, err := websocket.NewConfig(endpoint, "http://localhost")
	if err != nil {
		return nil, err
	}
	cfg.Origin = &url.URL{}
	cfg.Header.Set("Authorization", "Bearer isolated-test-token")
	return cfg.DialContext(ctx)
}
func fakeRunner(ctx context.Context, endpoint string, handle func(*websocket.Conn, map[string]json.RawMessage)) error {
	c, err := dialBot(ctx, endpoint+"/registry")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err = websocket.JSON.Send(c, map[string]any{"type": "hello", "protocol": 1, "runner_id": "go-runner", "bot_instance": "go-bot", "agent_name": "bridge", "config_revision": "1", "capacity": 1}); err != nil {
		return err
	}
	for {
		var m map[string]json.RawMessage
		if err = websocket.JSON.Receive(c, &m); err != nil {
			return err
		}
		var kind string
		_ = json.Unmarshal(m["type"], &kind)
		switch kind {
		case "reserve":
			if err = websocket.JSON.Send(c, map[string]any{"type": "reserved", "reservation_id": m["reservation_id"], "accepted": true}); err != nil {
				return err
			}
		case "assign":
			conn, e := dialBot(ctx, endpoint)
			if e != nil {
				return e
			}
			go func() {
				defer func() { _ = conn.Close() }()
				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stop()
				_ = conn.SetDeadline(time.Now().Add(12 * time.Second))

				handle(conn, m)
			}()
		}
	}
}

func attachFake(conn *websocket.Conn, m map[string]json.RawMessage, last uint64) error {
	return websocket.JSON.Send(conn, map[string]any{"type": "attach", "protocol": 1, "session_id": m["session_id"], "epoch": m["epoch"], "generation": m["generation"], "bot_instance": m["bot_instance"], "attach_token": m["attach_token"], "last_output_seq": last})
}
