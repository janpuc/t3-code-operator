package t3client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAuthTokenManagerCleansStaleStateAndRenewsBeforeExpiry(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	clock := &fakeAuthClock{now: now}
	store := newFakeAuthStore()
	store.sessions["stale-bootstrap"] = authStoredSession{ID: "stale-bootstrap", Label: "t3-coded-bootstrap-workstation-uid"}
	store.sessions["stale-narrow"] = authStoredSession{ID: "stale-narrow", Label: "t3-coded-workstation-uid"}
	store.pairings["stale-pairing"] = authStoredPairing{ID: "stale-pairing", Label: "t3-coded-workstation-uid"}
	backend := newFakeAuthBackend(t, store, 90)
	defer backend.Close()
	manager := newTestAuthTokenManager(t, backend.URL, store, clock)

	first, err := manager.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "access-1" {
		t.Fatalf("unexpected first token %q", first)
	}
	assertOnlyNarrowSession(t, store, "narrow-1")
	if _, exists := store.pairings["stale-pairing"]; exists {
		t.Fatal("stale pairing was not revoked")
	}

	clock.now = now.Add(59 * time.Second)
	unchanged, err := manager.Token(context.Background())
	if err != nil || unchanged != first {
		t.Fatalf("token renewed too early: token=%q err=%v", unchanged, err)
	}
	clock.now = now.Add(61 * time.Second)
	second, err := manager.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second != "access-2" || second == first {
		t.Fatalf("token did not renew: first=%q second=%q", first, second)
	}
	assertOnlyNarrowSession(t, store, "narrow-2")
	if _, exists := store.sessions["narrow-1"]; exists {
		t.Fatal("prior narrow session was not revoked")
	}

	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.sessions) != 0 || len(store.pairings) != 0 {
		t.Fatalf("auth cleanup left state: sessions=%#v pairings=%#v", store.sessions, store.pairings)
	}
}

func TestAuthRenewalFailureBlocksTokenUseWithoutRevokingCurrentSession(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	clock := &fakeAuthClock{now: now}
	store := newFakeAuthStore()
	backendState := &fakeAuthBackendState{expiresIn: 90}
	backend := newFakeAuthBackendWithState(t, store, backendState)
	defer backend.Close()
	manager := newTestAuthTokenManager(t, backend.URL, store, clock)
	first, err := manager.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clock.now = now.Add(61 * time.Second)
	backendState.mutex.Lock()
	backendState.failExchange = true
	backendState.mutex.Unlock()
	if _, err := manager.Token(context.Background()); err == nil {
		t.Fatal("expected renewal failure")
	}
	if manager.token != first || manager.sessionID != "narrow-1" {
		t.Fatalf("renewal failure discarded the current session: token=%q session=%q", manager.token, manager.sessionID)
	}
	if _, exists := store.sessions["narrow-1"]; !exists {
		t.Fatal("renewal failure revoked the current session")
	}
	backendState.mutex.Lock()
	backendState.failExchange = false
	backendState.mutex.Unlock()
	second, err := manager.Token(context.Background())
	if err != nil || second != "access-2" {
		t.Fatalf("renewal did not recover: token=%q err=%v", second, err)
	}
}

func TestAuthRenewalRevokesAConsumedPairingThatUpstreamRetains(t *testing.T) {
	clock := &fakeAuthClock{now: time.Now()}
	store := newFakeAuthStore()
	backend := newFakeAuthBackendWithState(t, store, &fakeAuthBackendState{expiresIn: 90, retainPairing: true})
	defer backend.Close()
	manager := newTestAuthTokenManager(t, backend.URL, store, clock)
	if _, err := manager.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.pairings) != 0 {
		t.Fatalf("successful renewal retained pairing state: %#v", store.pairings)
	}
}

func TestAuthErrorsDoNotDiscloseCredentials(t *testing.T) {
	clock := &fakeAuthClock{now: time.Now()}
	store := newFakeAuthStore()
	store.issueError = errors.New("bootstrap-secret-canary")
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	manager := newTestAuthTokenManager(t, server.URL, store, clock)
	_, err := manager.Token(context.Background())
	if err == nil || strings.Contains(err.Error(), "bootstrap-secret-canary") {
		t.Fatalf("auth error disclosed a credential: %v", err)
	}
}

func newTestAuthTokenManager(
	t *testing.T,
	baseURL string,
	store authStore,
	clock authClock,
) *AuthTokenManager {
	t.Helper()
	manager, err := newAuthTokenManager(AuthConfig{
		BaseURL:        baseURL,
		BaseDirectory:  t.TempDir(),
		ClientID:       "workstation-uid",
		RequestTimeout: 5 * time.Second,
		RenewBefore:    30 * time.Second,
	}, store, clock)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func assertOnlyNarrowSession(t *testing.T, store *fakeAuthStore, expectedID string) {
	t.Helper()
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if len(store.sessions) != 1 {
		t.Fatalf("unexpected sessions: %#v", store.sessions)
	}
	session, exists := store.sessions[expectedID]
	if !exists || session.Label != "t3-coded-workstation-uid" {
		t.Fatalf("unexpected narrow session: %#v", store.sessions)
	}
}

type fakeAuthClock struct {
	now time.Time
}

func (clock *fakeAuthClock) Now() time.Time {
	return clock.now
}

type fakeAuthStore struct {
	mutex           sync.Mutex
	sessions        map[string]authStoredSession
	pairings        map[string]authStoredPairing
	bootstrapTokens map[string]string
	issueCount      int
	issueError      error
}

func newFakeAuthStore() *fakeAuthStore {
	return &fakeAuthStore{
		sessions:        map[string]authStoredSession{},
		pairings:        map[string]authStoredPairing{},
		bootstrapTokens: map[string]string{},
	}
}

func (store *fakeAuthStore) IssueBootstrap(_ context.Context, label string) (authBootstrapSession, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if store.issueError != nil {
		return authBootstrapSession{}, store.issueError
	}
	store.issueCount++
	id := "bootstrap-" + strconv.Itoa(store.issueCount)
	token := "bootstrap-token-" + strconv.Itoa(store.issueCount)
	store.sessions[id] = authStoredSession{ID: id, Label: label}
	store.bootstrapTokens[token] = id
	return authBootstrapSession{
		ID:     id,
		Token:  token,
		Scopes: []string{"access:read", "access:write"},
	}, nil
}

func (store *fakeAuthStore) ListSessions(context.Context) ([]authStoredSession, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	result := make([]authStoredSession, 0, len(store.sessions))
	for _, session := range store.sessions {
		result = append(result, session)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (store *fakeAuthStore) RevokeSession(_ context.Context, id string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	delete(store.sessions, id)
	for token, sessionID := range store.bootstrapTokens {
		if sessionID == id {
			delete(store.bootstrapTokens, token)
		}
	}
	return nil
}

func (store *fakeAuthStore) ListPairings(context.Context) ([]authStoredPairing, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	result := make([]authStoredPairing, 0, len(store.pairings))
	for _, pairing := range store.pairings {
		result = append(result, pairing)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (store *fakeAuthStore) RevokePairing(_ context.Context, id string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	delete(store.pairings, id)
	return nil
}

type fakeAuthBackendState struct {
	mutex         sync.Mutex
	expiresIn     int
	accessCount   int
	failExchange  bool
	retainPairing bool
	accessTokens  map[string]string
}

func newFakeAuthBackend(t *testing.T, store *fakeAuthStore, expiresIn int) *httptest.Server {
	return newFakeAuthBackendWithState(t, store, &fakeAuthBackendState{expiresIn: expiresIn})
}

func newFakeAuthBackendWithState(
	t *testing.T,
	store *fakeAuthStore,
	state *fakeAuthBackendState,
) *httptest.Server {
	t.Helper()
	state.accessTokens = map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/pairing-token", func(response http.ResponseWriter, request *http.Request) {
		bootstrapToken := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		store.mutex.Lock()
		_, valid := store.bootstrapTokens[bootstrapToken]
		store.mutex.Unlock()
		if request.Method != http.MethodPost || !valid {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			Label  string   `json:"label"`
			Scopes []string `json:"scopes"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		store.mutex.Lock()
		id := "pairing-" + strconv.Itoa(store.issueCount)
		credential := "pairing-credential-" + strconv.Itoa(store.issueCount)
		store.pairings[id] = authStoredPairing{ID: id, Label: body.Label}
		store.mutex.Unlock()
		writeAuthJSON(response, map[string]any{
			"id": id, "credential": credential, "label": body.Label, "expiresAt": "2026-08-27T13:00:00Z",
		})
	})
	mux.HandleFunc("/oauth/token", func(response http.ResponseWriter, request *http.Request) {
		state.mutex.Lock()
		defer state.mutex.Unlock()
		if state.failExchange {
			response.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if err := request.ParseForm(); err != nil || request.Form.Get("subject_token") == "" {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		state.accessCount++
		accessToken := "access-" + strconv.Itoa(state.accessCount)
		sessionID := "narrow-" + strconv.Itoa(state.accessCount)
		label := request.Form.Get("client_label")
		state.accessTokens[accessToken] = sessionID
		store.mutex.Lock()
		store.sessions[sessionID] = authStoredSession{ID: sessionID, Label: label}
		if !state.retainPairing {
			for id, pairing := range store.pairings {
				if pairing.Label == label {
					delete(store.pairings, id)
				}
			}
		}
		store.mutex.Unlock()
		writeAuthJSON(response, map[string]any{
			"access_token":      accessToken,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
			"token_type":        "Bearer",
			"expires_in":        state.expiresIn,
			"scope":             "orchestration:read orchestration:operate",
		})
	})
	mux.HandleFunc("/api/auth/session", func(response http.ResponseWriter, request *http.Request) {
		accessToken := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		state.mutex.Lock()
		sessionID, valid := state.accessTokens[accessToken]
		state.mutex.Unlock()
		store.mutex.Lock()
		_, exists := store.sessions[sessionID]
		store.mutex.Unlock()
		writeAuthJSON(response, map[string]any{
			"authenticated": valid && exists,
			"scopes":        []string{"orchestration:read", "orchestration:operate"},
		})
	})
	return httptest.NewServer(mux)
}

func writeAuthJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(value)
}
