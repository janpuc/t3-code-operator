package t3client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/janpuc/t3-code-operator/internal/apply"
)

const (
	defaultRequestTimeout = 30 * time.Second
	maxHTTPResponseBytes  = 16 << 20
	maxRPCMessageBytes    = 8 << 20
	maxTicketBytes        = 64 << 10
)

var errManagedSettingsDiffer = errors.New("upstream managed settings differ")

type TokenSource interface {
	Token(context.Context) (string, error)
}

type StaticTokenSource string

func (source StaticTokenSource) Token(context.Context) (string, error) {
	return string(source), nil
}

type Config struct {
	BaseURL        string
	Tokens         TokenSource
	HTTPClient     *http.Client
	RequestTimeout time.Duration
}

type Client struct {
	baseURL        *url.URL
	tokens         TokenSource
	httpClient     *http.Client
	requestTimeout time.Duration
}

func New(config Config) (*Client, error) {
	if config.Tokens == nil {
		return nil, errors.New("t3 bearer token source is required")
	}
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("t3 base URL is invalid")
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, errors.New("t3 base URL must use HTTP or HTTPS")
	}
	if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" || (baseURL.Path != "" && baseURL.Path != "/") {
		return nil, errors.New("t3 base URL must not contain credentials, a path, a query, or a fragment")
	}
	if !isLoopbackHost(baseURL.Hostname()) {
		return nil, errors.New("t3 base URL must use a loopback host")
	}
	baseURL.Path = ""
	requestTimeout := config.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	configuredHTTPClient := config.HTTPClient
	if configuredHTTPClient == nil {
		configuredHTTPClient = http.DefaultClient
	}
	httpClient := *configuredHTTPClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("t3 client refuses HTTP redirects")
	}
	return &Client{
		baseURL:        baseURL,
		tokens:         config.Tokens,
		httpClient:     &httpClient,
		requestTimeout: requestTimeout,
	}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func (client *Client) ApplyManagedSettings(
	ctx context.Context,
	settings apply.ManagedSettings,
) error {
	payload := struct {
		Patch apply.ManagedSettings `json:"patch"`
	}{Patch: settings}
	result, err := client.rpc(ctx, "server.updateSettings", payload)
	if err != nil {
		return fmt.Errorf("update upstream managed settings: %w", err)
	}
	if err := verifyManagedSettings(result, settings); err != nil {
		return fmt.Errorf("verify upstream managed settings: %w", err)
	}
	return nil
}

func (client *Client) ManagedSettingsMatch(
	ctx context.Context,
	settings apply.ManagedSettings,
) (bool, error) {
	result, err := client.rpc(ctx, "server.getSettings", struct{}{})
	if err != nil {
		return false, fmt.Errorf("read upstream managed settings: %w", err)
	}
	if err := verifyManagedSettings(result, settings); err != nil {
		if errors.Is(err, errManagedSettingsDiffer) {
			return false, nil
		}
		return false, fmt.Errorf("verify upstream managed settings: %w", err)
	}
	return true, nil
}

func (client *Client) rpc(ctx context.Context, tag string, payload any) (json.RawMessage, error) {
	requestContext, cancel := client.withTimeout(ctx)
	defer cancel()
	token, err := client.bearerToken(requestContext)
	if err != nil {
		return nil, err
	}
	ticket, err := client.websocketTicket(requestContext, token)
	if err != nil {
		return nil, err
	}
	websocketURL := client.endpoint("/ws")
	if websocketURL.Scheme == "https" {
		websocketURL.Scheme = "wss"
	} else {
		websocketURL.Scheme = "ws"
	}
	query := websocketURL.Query()
	query.Set("wsTicket", ticket)
	websocketURL.RawQuery = query.Encode()
	connection, _, err := websocket.Dial(requestContext, websocketURL.String(), &websocket.DialOptions{
		HTTPClient:      client.httpClient,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, errors.New("connect to t3 RPC WebSocket")
	}
	defer connection.CloseNow()
	connection.SetReadLimit(maxRPCMessageBytes)

	const requestID int64 = 1
	request := struct {
		Tag     string `json:"_tag"`
		ID      int64  `json:"id"`
		Method  string `json:"tag"`
		Payload any    `json:"payload"`
		Headers []any  `json:"headers"`
	}{
		Tag:     "Request",
		ID:      requestID,
		Method:  tag,
		Payload: payload,
		Headers: []any{},
	}
	if err := wsjson.Write(requestContext, connection, request); err != nil {
		return nil, errors.New("send t3 RPC request")
	}
	for {
		_, raw, err := connection.Read(requestContext)
		if err != nil {
			return nil, errors.New("read t3 RPC response")
		}
		messages, err := decodeRPCMessages(raw)
		if err != nil {
			return nil, err
		}
		for _, message := range messages {
			var envelope struct {
				Tag       string `json:"_tag"`
				RequestID int64  `json:"requestId"`
				Exit      struct {
					Tag   string          `json:"_tag"`
					Value json.RawMessage `json:"value"`
				} `json:"exit"`
			}
			if err := json.Unmarshal(message, &envelope); err != nil {
				return nil, errors.New("t3 RPC returned an invalid message")
			}
			switch envelope.Tag {
			case "Ping":
				if err := wsjson.Write(requestContext, connection, map[string]string{"_tag": "Pong"}); err != nil {
					return nil, errors.New("send t3 RPC Pong")
				}
			case "Defect":
				return nil, errors.New("t3 RPC returned a protocol defect")
			case "Exit":
				if envelope.RequestID != requestID {
					continue
				}
				if envelope.Exit.Tag != "Success" {
					return nil, fmt.Errorf("t3 RPC rejected %s", tag)
				}
				if len(envelope.Exit.Value) == 0 || !json.Valid(envelope.Exit.Value) {
					return nil, errors.New("t3 RPC returned an invalid success value")
				}
				return append(json.RawMessage(nil), envelope.Exit.Value...), nil
			}
		}
	}
}

type redactedProviderInstance struct {
	Driver      string                        `json:"driver"`
	DisplayName string                        `json:"displayName,omitempty"`
	AccentColor string                        `json:"accentColor,omitempty"`
	Enabled     bool                          `json:"enabled"`
	Environment []redactedProviderEnvironment `json:"environment,omitempty"`
	Config      json.RawMessage               `json:"config,omitempty"`
}

type redactedProviderEnvironment struct {
	Name          string `json:"name"`
	Value         string `json:"value,omitempty"`
	Sensitive     bool   `json:"sensitive,omitempty"`
	ValueRedacted bool   `json:"valueRedacted,omitempty"`
}

func verifyManagedSettings(raw json.RawMessage, desired apply.ManagedSettings) error {
	var settings struct {
		EnableProviderUpdateChecks *bool                               `json:"enableProviderUpdateChecks"`
		ProviderInstances          map[string]redactedProviderInstance `json:"providerInstances"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil || settings.ProviderInstances == nil || settings.EnableProviderUpdateChecks == nil {
		return errors.New("upstream returned an invalid settings snapshot")
	}
	if *settings.EnableProviderUpdateChecks != desired.EnableProviderUpdateChecks {
		return fmt.Errorf("%w: upstream provider update checks do not match", errManagedSettingsDiffer)
	}
	if len(settings.ProviderInstances) != len(desired.ProviderInstances) {
		return errManagedSettingsDiffer
	}
	instanceIDs := make([]string, 0, len(desired.ProviderInstances))
	for instanceID := range desired.ProviderInstances {
		instanceIDs = append(instanceIDs, instanceID)
	}
	sort.Strings(instanceIDs)
	for _, instanceID := range instanceIDs {
		actual, exists := settings.ProviderInstances[instanceID]
		if !exists {
			return fmt.Errorf("%w: upstream omitted provider instance %s", errManagedSettingsDiffer, instanceID)
		}
		if err := verifyProviderInstance(actual, desired.ProviderInstances[instanceID]); err != nil {
			return fmt.Errorf("%w: upstream changed provider instance %s: %v", errManagedSettingsDiffer, instanceID, err)
		}
	}
	return nil
}

func verifyProviderInstance(actual redactedProviderInstance, desired apply.ProviderInstance) error {
	if actual.Driver != desired.Driver || actual.DisplayName != desired.DisplayName ||
		actual.AccentColor != desired.AccentColor || actual.Enabled != desired.Enabled {
		return errors.New("identity fields do not match")
	}
	if !jsonValuesEqual(actual.Config, desired.Config) {
		return errors.New("config does not match")
	}
	if len(actual.Environment) != len(desired.Environment) {
		return errors.New("environment does not match")
	}
	desiredEnvironment := make(map[string]apply.ProviderEnvironment, len(desired.Environment))
	for _, variable := range desired.Environment {
		if _, exists := desiredEnvironment[variable.Name]; exists {
			return errors.New("desired environment has a duplicate name")
		}
		desiredEnvironment[variable.Name] = variable
	}
	for _, variable := range actual.Environment {
		expected, exists := desiredEnvironment[variable.Name]
		if !exists {
			return errors.New("environment contains an unexpected name")
		}
		delete(desiredEnvironment, variable.Name)
		if variable.Sensitive != expected.Sensitive {
			return errors.New("environment sensitivity does not match")
		}
		if expected.Sensitive {
			if variable.Value != "" || !variable.ValueRedacted {
				return errors.New("sensitive environment value was not redacted")
			}
		} else if variable.Value != expected.Value || variable.ValueRedacted {
			return errors.New("environment value does not match")
		}
	}
	if len(desiredEnvironment) != 0 {
		return errors.New("environment omitted a desired name")
	}
	return nil
}

func jsonValuesEqual(left, right json.RawMessage) bool {
	if len(bytes.TrimSpace(left)) == 0 || len(bytes.TrimSpace(right)) == 0 {
		return len(bytes.TrimSpace(left)) == 0 && len(bytes.TrimSpace(right)) == 0
	}
	var leftValue any
	leftDecoder := json.NewDecoder(bytes.NewReader(left))
	leftDecoder.UseNumber()
	if err := leftDecoder.Decode(&leftValue); err != nil {
		return false
	}
	var rightValue any
	rightDecoder := json.NewDecoder(bytes.NewReader(right))
	rightDecoder.UseNumber()
	if err := rightDecoder.Decode(&rightValue); err != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func decodeRPCMessages(raw []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("t3 RPC returned an empty message")
	}
	if trimmed[0] != '[' {
		if !json.Valid(trimmed) {
			return nil, errors.New("t3 RPC returned invalid JSON")
		}
		return []json.RawMessage{append([]byte(nil), trimmed...)}, nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(trimmed, &messages); err != nil || len(messages) == 0 {
		return nil, errors.New("t3 RPC returned an invalid message batch")
	}
	return messages, nil
}

func (client *Client) websocketTicket(ctx context.Context, token string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint("/api/auth/websocket-ticket").String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.httpClient.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return "", errors.New("request t3 WebSocket ticket")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request t3 WebSocket ticket: HTTP status %d", response.StatusCode)
	}
	raw, err := readBoundedResponse(response.Body, maxTicketBytes)
	if err != nil {
		return "", fmt.Errorf("read t3 WebSocket ticket: %w", err)
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || !validBearerToken(body.Ticket) {
		return "", errors.New("t3 WebSocket ticket response is invalid")
	}
	return body.Ticket, nil
}

func (client *Client) bearerToken(ctx context.Context) (string, error) {
	token, err := client.tokens.Token(ctx)
	if err != nil {
		return "", errors.New("get t3 bearer token")
	}
	if !validBearerToken(token) {
		return "", errors.New("t3 bearer token is invalid")
	}
	return token, nil
}

func validBearerToken(token string) bool {
	return token != "" && strings.TrimSpace(token) == token && !strings.ContainsAny(token, "\x00\r\n\t ")
}

func (client *Client) endpoint(path string) *url.URL {
	result := *client.baseURL
	result.Path = path
	result.RawPath = ""
	result.RawQuery = ""
	result.Fragment = ""
	return &result
}

func (client *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, client.requestTimeout)
}

func readBoundedResponse(reader io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("response exceeded its size limit")
	}
	return raw, nil
}
