package t3client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultAuthRenewBefore = 5 * time.Minute
	maxAuthResponseBytes   = 1 << 20
)

var authClientIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

var requiredAuthScopes = []string{"orchestration:operate", "orchestration:read"}

type AuthConfig struct {
	BaseURL        string
	BaseDirectory  string
	ClientID       string
	T3Binary       string
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	RenewBefore    time.Duration
}

type AuthTokenManager struct {
	mutex sync.Mutex

	client         *Client
	store          authStore
	clock          authClock
	bootstrapLabel string
	narrowLabel    string
	renewBefore    time.Duration

	initialized bool
	token       string
	sessionID   string
	expiresAt   time.Time
	renewAt     time.Time
}

type authClock interface {
	Now() time.Time
}

type realAuthClock struct{}

func (realAuthClock) Now() time.Time {
	return time.Now()
}

type authBootstrapSession struct {
	ID     string
	Token  string
	Scopes []string
}

type authStoredSession struct {
	ID    string
	Label string
}

type authStoredPairing struct {
	ID    string
	Label string
}

type authStore interface {
	IssueBootstrap(context.Context, string) (authBootstrapSession, error)
	ListSessions(context.Context) ([]authStoredSession, error)
	RevokeSession(context.Context, string) error
	ListPairings(context.Context) ([]authStoredPairing, error)
	RevokePairing(context.Context, string) error
}

func NewAuthTokenManager(config AuthConfig) (*AuthTokenManager, error) {
	binary := config.T3Binary
	if binary == "" {
		binary = "t3"
	}
	commandTimeout := config.RequestTimeout
	if commandTimeout <= 0 {
		commandTimeout = defaultRequestTimeout
	}
	store := &commandAuthStore{
		binary:        binary,
		baseDirectory: filepath.Clean(config.BaseDirectory),
		timeout:       commandTimeout,
	}
	return newAuthTokenManager(config, store, realAuthClock{})
}

func newAuthTokenManager(config AuthConfig, store authStore, clock authClock) (*AuthTokenManager, error) {
	if config.BaseDirectory == "" || !filepath.IsAbs(config.BaseDirectory) {
		return nil, errors.New("t3 base directory must be an absolute path")
	}
	baseDirectory := filepath.Clean(config.BaseDirectory)
	info, err := os.Stat(baseDirectory)
	if err != nil || !info.IsDir() {
		return nil, errors.New("t3 base directory must exist")
	}
	if !authClientIDPattern.MatchString(config.ClientID) {
		return nil, errors.New("t3 auth client ID is invalid")
	}
	if store == nil || clock == nil {
		return nil, errors.New("t3 auth store and clock are required")
	}
	transport, err := New(Config{
		BaseURL:        config.BaseURL,
		Tokens:         StaticTokenSource("unused"),
		HTTPClient:     config.HTTPClient,
		RequestTimeout: config.RequestTimeout,
	})
	if err != nil {
		return nil, err
	}
	renewBefore := config.RenewBefore
	if renewBefore <= 0 {
		renewBefore = defaultAuthRenewBefore
	}
	return &AuthTokenManager{
		client:         transport,
		store:          store,
		clock:          clock,
		bootstrapLabel: "t3-coded-bootstrap-" + config.ClientID,
		narrowLabel:    "t3-coded-" + config.ClientID,
		renewBefore:    renewBefore,
	}, nil
}

func (manager *AuthTokenManager) Token(ctx context.Context) (string, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if !manager.initialized {
		if err := manager.cleanupStale(ctx); err != nil {
			return "", errors.New("clean stale t3 auth state")
		}
		manager.initialized = true
	}
	if manager.token != "" && manager.clock.Now().Before(manager.renewAt) {
		return manager.token, nil
	}
	if err := manager.renew(ctx); err != nil {
		return "", errors.New("renew t3 auth session")
	}
	return manager.token, nil
}

func (manager *AuthTokenManager) Close(ctx context.Context) error {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if err := manager.cleanupStale(ctx); err != nil {
		return errors.New("clean t3 auth state")
	}
	manager.initialized = false
	manager.token = ""
	manager.sessionID = ""
	manager.expiresAt = time.Time{}
	manager.renewAt = time.Time{}
	return nil
}

func (manager *AuthTokenManager) cleanupStale(ctx context.Context) error {
	pairings, err := manager.store.ListPairings(ctx)
	if err != nil {
		return err
	}
	for _, pairing := range pairings {
		if pairing.Label != manager.narrowLabel {
			continue
		}
		if err := manager.store.RevokePairing(ctx, pairing.ID); err != nil {
			return err
		}
	}
	sessions, err := manager.store.ListSessions(ctx)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		if session.Label != manager.narrowLabel && session.Label != manager.bootstrapLabel {
			continue
		}
		if err := manager.store.RevokeSession(ctx, session.ID); err != nil {
			return err
		}
	}
	return nil
}

func (manager *AuthTokenManager) renew(ctx context.Context) error {
	bootstrap, err := manager.store.IssueBootstrap(ctx, manager.bootstrapLabel)
	if err != nil {
		return err
	}
	if bootstrap.ID == "" || !validBearerToken(bootstrap.Token) ||
		!containsAllScopes(bootstrap.Scopes, []string{"access:read", "access:write"}) {
		_ = manager.store.RevokeSession(context.WithoutCancel(ctx), bootstrap.ID)
		return errors.New("bootstrap session is invalid")
	}
	pairing, err := manager.createPairing(ctx, bootstrap.Token)
	if err != nil {
		_ = manager.store.RevokeSession(context.WithoutCancel(ctx), bootstrap.ID)
		return err
	}
	access, err := manager.exchangePairing(ctx, pairing.Credential)
	if err != nil {
		cleanupContext := context.WithoutCancel(ctx)
		_ = manager.store.RevokePairing(cleanupContext, pairing.ID)
		_ = manager.store.RevokeSession(cleanupContext, bootstrap.ID)
		return err
	}
	if err := manager.verifyNarrowSession(ctx, access.Token); err != nil {
		manager.cleanupFailedAccess(context.WithoutCancel(ctx), bootstrap.ID, pairing.ID)
		return err
	}
	newSessionID, err := manager.findNewNarrowSession(ctx)
	if err != nil {
		manager.cleanupFailedAccess(context.WithoutCancel(ctx), bootstrap.ID, pairing.ID)
		return err
	}
	cleanupContext := context.WithoutCancel(ctx)
	if err := manager.revokePairingIfPresent(cleanupContext, pairing.ID); err != nil {
		manager.cleanupFailedAccess(cleanupContext, bootstrap.ID, pairing.ID)
		return err
	}
	if err := manager.store.RevokeSession(cleanupContext, bootstrap.ID); err != nil {
		_ = manager.store.RevokeSession(cleanupContext, newSessionID)
		return err
	}
	if manager.sessionID != "" {
		if err := manager.store.RevokeSession(cleanupContext, manager.sessionID); err != nil {
			_ = manager.store.RevokeSession(cleanupContext, newSessionID)
			return err
		}
	}
	issuedAt := manager.clock.Now()
	ttl := time.Duration(access.ExpiresIn) * time.Second
	renewBefore := manager.renewBefore
	if renewBefore >= ttl {
		renewBefore = ttl / 3
	}
	manager.token = access.Token
	manager.sessionID = newSessionID
	manager.expiresAt = issuedAt.Add(ttl)
	manager.renewAt = manager.expiresAt.Add(-renewBefore)
	return nil
}

func (manager *AuthTokenManager) revokePairingIfPresent(ctx context.Context, pairingID string) error {
	pairings, err := manager.store.ListPairings(ctx)
	if err != nil {
		return err
	}
	for _, pairing := range pairings {
		if pairing.ID == pairingID {
			return manager.store.RevokePairing(ctx, pairingID)
		}
	}
	return nil
}

type authPairingCredential struct {
	ID         string
	Credential string
}

type authAccessToken struct {
	Token     string
	ExpiresIn int64
}

func (manager *AuthTokenManager) createPairing(ctx context.Context, bootstrapToken string) (authPairingCredential, error) {
	body, err := json.Marshal(map[string]any{
		"label":  manager.narrowLabel,
		"scopes": requiredAuthScopes,
	})
	if err != nil {
		return authPairingCredential{}, err
	}
	raw, err := manager.authRequest(
		ctx,
		http.MethodPost,
		"/api/auth/pairing-token",
		"application/json",
		bytes.NewReader(body),
		bootstrapToken,
	)
	if err != nil {
		return authPairingCredential{}, err
	}
	var response struct {
		ID         string `json:"id"`
		Credential string `json:"credential"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || response.ID == "" || !validBearerToken(response.Credential) {
		return authPairingCredential{}, errors.New("pairing response is invalid")
	}
	return authPairingCredential{ID: response.ID, Credential: response.Credential}, nil
}

func (manager *AuthTokenManager) exchangePairing(ctx context.Context, credential string) (authAccessToken, error) {
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {credential},
		"subject_token_type":   {"urn:t3:params:oauth:token-type:environment-bootstrap"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"scope":                {strings.Join(requiredAuthScopes, " ")},
		"client_label":         {manager.narrowLabel},
	}
	raw, err := manager.authRequest(
		ctx,
		http.MethodPost,
		"/oauth/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()),
		"",
	)
	if err != nil {
		return authAccessToken{}, err
	}
	var response struct {
		AccessToken string  `json:"access_token"`
		TokenType   string  `json:"token_type"`
		ExpiresIn   float64 `json:"expires_in"`
		Scope       string  `json:"scope"`
	}
	if err := json.Unmarshal(raw, &response); err != nil ||
		response.TokenType != "Bearer" ||
		!validBearerToken(response.AccessToken) ||
		response.ExpiresIn <= 0 || response.ExpiresIn > 365*24*60*60 ||
		response.ExpiresIn != float64(int64(response.ExpiresIn)) ||
		!scopesEqual(strings.Fields(response.Scope), requiredAuthScopes) {
		return authAccessToken{}, errors.New("access token response is invalid")
	}
	return authAccessToken{Token: response.AccessToken, ExpiresIn: int64(response.ExpiresIn)}, nil
}

func (manager *AuthTokenManager) verifyNarrowSession(ctx context.Context, token string) error {
	raw, err := manager.authRequest(ctx, http.MethodGet, "/api/auth/session", "", nil, token)
	if err != nil {
		return err
	}
	var response struct {
		Authenticated bool     `json:"authenticated"`
		Scopes        []string `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &response); err != nil ||
		!response.Authenticated || !scopesEqual(response.Scopes, requiredAuthScopes) {
		return errors.New("narrow session verification failed")
	}
	return nil
}

func (manager *AuthTokenManager) findNewNarrowSession(ctx context.Context) (string, error) {
	sessions, err := manager.store.ListSessions(ctx)
	if err != nil {
		return "", err
	}
	candidates := make([]string, 0, 1)
	for _, session := range sessions {
		if session.Label == manager.narrowLabel && session.ID != manager.sessionID {
			candidates = append(candidates, session.ID)
		}
	}
	if len(candidates) != 1 {
		return "", errors.New("new narrow session cannot be identified")
	}
	return candidates[0], nil
}

func (manager *AuthTokenManager) cleanupFailedAccess(ctx context.Context, bootstrapID, pairingID string) {
	_ = manager.store.RevokePairing(ctx, pairingID)
	_ = manager.store.RevokeSession(ctx, bootstrapID)
	sessions, err := manager.store.ListSessions(ctx)
	if err != nil {
		return
	}
	for _, session := range sessions {
		if session.Label == manager.narrowLabel && session.ID != manager.sessionID {
			_ = manager.store.RevokeSession(ctx, session.ID)
		}
	}
}

func (manager *AuthTokenManager) authRequest(
	ctx context.Context,
	method string,
	path string,
	contentType string,
	body io.Reader,
	bearer string,
) ([]byte, error) {
	requestContext, cancel := manager.client.withTimeout(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, method, manager.client.endpoint(path).String(), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := manager.client.httpClient.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, errors.New("t3 auth request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("t3 auth request failed with HTTP status %d", response.StatusCode)
	}
	raw, err := readBoundedResponse(response.Body, maxAuthResponseBytes)
	if err != nil {
		return nil, errors.New("t3 auth response is invalid")
	}
	return raw, nil
}

func scopesEqual(left, right []string) bool {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsAllScopes(actual, required []string) bool {
	available := map[string]struct{}{}
	for _, scope := range actual {
		available[scope] = struct{}{}
	}
	for _, scope := range required {
		if _, exists := available[scope]; !exists {
			return false
		}
	}
	return true
}
