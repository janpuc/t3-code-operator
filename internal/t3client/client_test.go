package t3client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/janpuc/t3-code-operator/internal/apply"
)

func TestApplyManagedSettingsUsesUpstreamWebSocketRPC(t *testing.T) {
	const token = "narrow-bearer"
	const enableProviderUpdateChecks = true
	requestSeen := make(chan rpcTestRequest, 1)
	server := newT3RPCServer(t, token, func(ctx context.Context, connection *websocket.Conn, request rpcTestRequest) {
		requestSeen <- request
		if err := wsjson.Write(ctx, connection, map[string]any{"_tag": "Ping"}); err != nil {
			t.Errorf("write Ping: %v", err)
			return
		}
		var pong map[string]any
		if err := wsjson.Read(ctx, connection, &pong); err != nil {
			t.Errorf("read Pong: %v", err)
			return
		}
		if pong["_tag"] != "Pong" {
			t.Errorf("unexpected Pong: %#v", pong)
			return
		}
		if err := wsjson.Write(ctx, connection, map[string]any{
			"_tag":      "Exit",
			"requestId": request.ID,
			"exit": map[string]any{
				"_tag": "Success",
				"value": map[string]any{"enableProviderUpdateChecks": enableProviderUpdateChecks, "providerInstances": map[string]any{
					"codex": map[string]any{
						"driver":  "codex",
						"enabled": true,
						"environment": []map[string]any{{
							"name":          "OPENAI_API_KEY",
							"value":         "",
							"sensitive":     true,
							"valueRedacted": true,
						}},
						"config": map[string]any{
							"homePath": "/data/harnesses/codex/codex",
							"unknown":  true,
						},
					},
				}},
			},
		}); err != nil {
			t.Errorf("write Exit: %v", err)
		}
	})
	defer server.Close()
	client := newTestClient(t, server.URL, token)
	providers := map[string]apply.ProviderInstance{
		"codex": {
			Driver:  "codex",
			Enabled: true,
			Environment: []apply.ProviderEnvironment{{
				Name:      "OPENAI_API_KEY",
				Value:     "resolved-secret-canary",
				Sensitive: true,
			}},
			Config: json.RawMessage(`{"homePath":"/data/harnesses/codex/codex","unknown":true}`),
		},
	}
	settings := managedSettings(providers)
	settings.EnableProviderUpdateChecks = enableProviderUpdateChecks
	if err := client.ApplyManagedSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	select {
	case request := <-requestSeen:
		if request.Tag != "server.updateSettings" || len(request.Headers) != 0 {
			t.Fatalf("unexpected RPC request: %#v", request)
		}
		var payload struct {
			Patch struct {
				EnableProviderUpdateChecks *bool                             `json:"enableProviderUpdateChecks"`
				ProviderInstances          map[string]apply.ProviderInstance `json:"providerInstances"`
			} `json:"patch"`
		}
		if err := json.Unmarshal(request.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(payload.Patch.ProviderInstances, providers) {
			t.Fatalf("provider patch changed: got=%#v want=%#v", payload.Patch.ProviderInstances, providers)
		}
		if payload.Patch.EnableProviderUpdateChecks == nil || *payload.Patch.EnableProviderUpdateChecks != enableProviderUpdateChecks {
			t.Fatal("provider update-check policy changed")
		}
	case <-time.After(time.Second):
		t.Fatal("upstream RPC request was not observed")
	}
}

func TestApplyManagedSettingsRejectsMismatchedSuccessSnapshot(t *testing.T) {
	const token = "narrow-bearer"
	const secret = "resolved-secret-canary"
	server := newT3RPCServer(t, token, func(ctx context.Context, connection *websocket.Conn, request rpcTestRequest) {
		_ = wsjson.Write(ctx, connection, map[string]any{
			"_tag":      "Exit",
			"requestId": request.ID,
			"exit": map[string]any{
				"_tag":  "Success",
				"value": map[string]any{"enableProviderUpdateChecks": false, "providerInstances": map[string]any{}},
			},
		})
	})
	defer server.Close()
	client := newTestClient(t, server.URL, token)
	err := client.ApplyManagedSettings(context.Background(), managedSettings(map[string]apply.ProviderInstance{
		"codex": {
			Driver:      "codex",
			Enabled:     true,
			Environment: []apply.ProviderEnvironment{{Name: "TOKEN", Value: secret, Sensitive: true}},
		},
	}))
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), token) {
		t.Fatalf("mismatched RPC result was accepted or disclosed sensitive data: %v", err)
	}
}

func TestManagedSettingsMatchUsesReadOnlySettingsRPC(t *testing.T) {
	for name, responseInstances := range map[string]map[string]any{
		"match": {
			"codex": map[string]any{
				"driver":  "codex",
				"enabled": true,
				"environment": []map[string]any{{
					"name":          "TOKEN",
					"value":         "",
					"sensitive":     true,
					"valueRedacted": true,
				}},
			},
		},
		"drift": {},
	} {
		t.Run(name, func(t *testing.T) {
			const token = "read-bearer"
			requestSeen := make(chan rpcTestRequest, 1)
			server := newT3RPCServer(t, token, func(ctx context.Context, connection *websocket.Conn, request rpcTestRequest) {
				requestSeen <- request
				_ = wsjson.Write(ctx, connection, map[string]any{
					"_tag":      "Exit",
					"requestId": request.ID,
					"exit": map[string]any{
						"_tag":  "Success",
						"value": map[string]any{"enableProviderUpdateChecks": false, "providerInstances": responseInstances},
					},
				})
			})
			defer server.Close()
			client := newTestClient(t, server.URL, token)
			matches, err := client.ManagedSettingsMatch(context.Background(), managedSettings(map[string]apply.ProviderInstance{
				"codex": {
					Driver:      "codex",
					Enabled:     true,
					Environment: []apply.ProviderEnvironment{{Name: "TOKEN", Value: "secret-canary", Sensitive: true}},
				},
			}))
			if err != nil {
				t.Fatal(err)
			}
			if matches != (name == "match") {
				t.Fatalf("unexpected match result %v", matches)
			}
			request := <-requestSeen
			if request.Tag != "server.getSettings" || len(request.Headers) != 0 {
				t.Fatalf("unexpected settings read request: %#v", request)
			}
		})
	}
}

func TestManagedSettingsMatchDetectsEnabledUpdateChecks(t *testing.T) {
	const token = "read-bearer"
	server := newT3RPCServer(t, token, func(ctx context.Context, connection *websocket.Conn, request rpcTestRequest) {
		_ = wsjson.Write(ctx, connection, map[string]any{
			"_tag":      "Exit",
			"requestId": request.ID,
			"exit": map[string]any{
				"_tag": "Success",
				"value": map[string]any{
					"enableProviderUpdateChecks": true,
					"providerInstances": map[string]any{
						"codex": map[string]any{"driver": "codex", "enabled": true},
					},
				},
			},
		})
	})
	defer server.Close()
	client := newTestClient(t, server.URL, token)
	matches, err := client.ManagedSettingsMatch(context.Background(), managedSettings(map[string]apply.ProviderInstance{
		"codex": {Driver: "codex", Enabled: true},
	}))
	if err != nil || matches {
		t.Fatalf("enabled provider update checks were not detected: matches=%v err=%v", matches, err)
	}
}

func TestApplyManagedSettingsDoesNotDiscloseRPCSecrets(t *testing.T) {
	const token = "narrow-bearer"
	const secret = "resolved-secret-canary"
	server := newT3RPCServer(t, token, func(ctx context.Context, connection *websocket.Conn, request rpcTestRequest) {
		_ = wsjson.Write(ctx, connection, map[string]any{
			"_tag":      "Exit",
			"requestId": request.ID,
			"exit": map[string]any{
				"_tag": "Failure",
				"cause": map[string]any{
					"message": secret,
				},
			},
		})
	})
	defer server.Close()
	client := newTestClient(t, server.URL, token)
	err := client.ApplyManagedSettings(context.Background(), managedSettings(map[string]apply.ProviderInstance{
		"codex": {
			Driver:      "codex",
			Enabled:     true,
			Environment: []apply.ProviderEnvironment{{Name: "TOKEN", Value: secret, Sensitive: true}},
		},
	}))
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), token) {
		t.Fatalf("RPC error disclosed sensitive data: %v", err)
	}
}

func TestActiveInstancesBlocksEveryVisibleKindOfWork(t *testing.T) {
	const token = "read-bearer"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/orchestration/shell", func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
			"snapshotSequence":7,
			"threads":[
				{"id":"idle","modelSelection":{"instanceId":"codex"},"latestTurn":{"state":"completed"},"session":{"status":"ready","activeTurnId":null},"hasPendingApprovals":false,"hasPendingUserInput":false,"backgroundLiveness":null},
				{"id":"turn","modelSelection":{"instanceId":"claude"},"latestTurn":{"state":"running"},"session":{"status":"running","providerInstanceId":"claude","activeTurnId":"turn-1"}},
				{"id":"approval","modelSelection":{"instanceId":"ui-created"},"latestTurn":{"state":"completed"},"session":{"status":"ready","activeTurnId":null},"hasPendingApprovals":true},
				{"id":"background","modelSelection":{"instanceId":"worker"},"latestTurn":{"state":"completed"},"session":{"status":"ready","activeTurnId":null},"backgroundLiveness":"monitoring"},
				{"id":"future","modelSelection":{"instanceId":"future"},"latestTurn":{"state":"queued"},"session":{"status":"ready","activeTurnId":null}}
			],
			"projects":[],
			"updatedAt":"2026-08-27T00:00:00.000Z"
		}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := newTestClient(t, server.URL, token)
	active, err := client.ActiveInstances(context.Background(), []string{"codex", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "future", "ui-created", "worker"}
	if !reflect.DeepEqual(active, want) {
		t.Fatalf("active instances: got=%#v want=%#v", active, want)
	}
}

func TestHTTPFailuresDoNotDiscloseBearerOrResponseBody(t *testing.T) {
	const token = "secret-bearer"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "server echoed "+token, http.StatusUnauthorized)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, token)
	_, err := client.ActiveInstances(context.Background(), nil)
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "server echoed") {
		t.Fatalf("HTTP error disclosed sensitive data: %v", err)
	}
}

func TestNewRejectsNonLoopbackServer(t *testing.T) {
	_, err := New(Config{BaseURL: "https://example.com", Tokens: StaticTokenSource("token")})
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("expected loopback validation, got %v", err)
	}
}

type rpcTestRequest struct {
	Tag     string          `json:"tag"`
	ID      int64           `json:"id"`
	Payload json.RawMessage `json:"payload"`
	Headers []any           `json:"headers"`
}

func managedSettings(providers map[string]apply.ProviderInstance) apply.ManagedSettings {
	return apply.ManagedSettings{
		EnableProviderUpdateChecks: false,
		ProviderInstances:          providers,
	}
}

func newT3RPCServer(
	t *testing.T,
	token string,
	handle func(context.Context, *websocket.Conn, rpcTestRequest),
) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/websocket-ticket", func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer "+token {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"ticket":"one-time-ticket"}`))
	})
	mux.HandleFunc("/ws", func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("wsTicket") != "one-time-ticket" {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		connection, err := websocket.Accept(response, request, nil)
		if err != nil {
			t.Errorf("accept WebSocket: %v", err)
			return
		}
		defer connection.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var rpcRequest rpcTestRequest
		if err := wsjson.Read(ctx, connection, &rpcRequest); err != nil {
			t.Errorf("read RPC request: %v", err)
			return
		}
		handle(ctx, connection, rpcRequest)
	})
	return httptest.NewServer(mux)
}

func newTestClient(t *testing.T, baseURL, token string) *Client {
	t.Helper()
	client, err := New(Config{
		BaseURL:        baseURL,
		Tokens:         StaticTokenSource(token),
		RequestTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
