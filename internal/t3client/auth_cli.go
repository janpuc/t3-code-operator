package t3client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"time"
)

const authCommandOutputLimit = 1 << 20

type commandAuthStore struct {
	binary        string
	baseDirectory string
	timeout       time.Duration
}

func (store *commandAuthStore) IssueBootstrap(ctx context.Context, label string) (authBootstrapSession, error) {
	raw, err := store.output(
		ctx,
		"auth", "session", "issue",
		"--base-dir", store.baseDirectory,
		"--ttl", "2m",
		"--label", label,
		"--subject", "t3-coded-bootstrap",
		"--json",
	)
	if err != nil {
		return authBootstrapSession{}, err
	}
	var session struct {
		SessionID string   `json:"sessionId"`
		Token     string   `json:"token"`
		Scopes    []string `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return authBootstrapSession{}, errors.New("parse t3 bootstrap session")
	}
	return authBootstrapSession{ID: session.SessionID, Token: session.Token, Scopes: session.Scopes}, nil
}

func (store *commandAuthStore) ListSessions(ctx context.Context) ([]authStoredSession, error) {
	raw, err := store.output(ctx, "auth", "session", "list", "--base-dir", store.baseDirectory, "--json")
	if err != nil {
		return nil, err
	}
	var sessions []struct {
		SessionID string `json:"sessionId"`
		Client    struct {
			Label string `json:"label"`
		} `json:"client"`
	}
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return nil, errors.New("parse t3 auth sessions")
	}
	result := make([]authStoredSession, 0, len(sessions))
	for _, session := range sessions {
		if session.SessionID == "" {
			return nil, errors.New("t3 auth session has no ID")
		}
		result = append(result, authStoredSession{ID: session.SessionID, Label: session.Client.Label})
	}
	return result, nil
}

func (store *commandAuthStore) RevokeSession(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("t3 auth session has no ID")
	}
	_, err := store.output(ctx, "auth", "session", "revoke", "--base-dir", store.baseDirectory, id)
	return err
}

func (store *commandAuthStore) ListPairings(ctx context.Context) ([]authStoredPairing, error) {
	raw, err := store.output(ctx, "auth", "pairing", "list", "--base-dir", store.baseDirectory, "--json")
	if err != nil {
		return nil, err
	}
	var pairings []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(raw, &pairings); err != nil {
		return nil, errors.New("parse t3 auth pairings")
	}
	result := make([]authStoredPairing, 0, len(pairings))
	for _, pairing := range pairings {
		if pairing.ID == "" {
			return nil, errors.New("t3 auth pairing has no ID")
		}
		result = append(result, authStoredPairing{ID: pairing.ID, Label: pairing.Label})
	}
	return result, nil
}

func (store *commandAuthStore) RevokePairing(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("t3 auth pairing has no ID")
	}
	_, err := store.output(ctx, "auth", "pairing", "revoke", "--base-dir", store.baseDirectory, id)
	return err
}

func (store *commandAuthStore) output(ctx context.Context, arguments ...string) ([]byte, error) {
	commandContext, cancel := context.WithTimeout(ctx, store.timeout)
	defer cancel()
	command := exec.CommandContext(commandContext, store.binary, arguments...)
	command.Dir = store.baseDirectory
	command.Env = authCommandEnvironment()
	command.Stdin = bytes.NewReader(nil)
	standardOutput := &authCommandBuffer{limit: authCommandOutputLimit}
	standardError := &authCommandBuffer{limit: authCommandOutputLimit}
	command.Stdout = standardOutput
	command.Stderr = standardError
	if err := command.Run(); err != nil {
		return nil, errors.New("t3 auth command failed")
	}
	if standardOutput.truncated {
		return nil, errors.New("t3 auth command output exceeded its size limit")
	}
	return append([]byte(nil), standardOutput.Bytes()...), nil
}

func authCommandEnvironment() []string {
	result := make([]string, 0, 8)
	for _, name := range []string{"PATH", "LANG", "LC_ALL", "TMPDIR"} {
		if value, exists := os.LookupEnv(name); exists {
			result = append(result, name+"="+value)
		}
	}
	return append(result, "CI=1", "NO_COLOR=1", "TERM=dumb")
}

type authCommandBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *authCommandBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := buffer.limit - buffer.Len()
	if len(value) > remaining {
		buffer.truncated = true
		if remaining < 0 {
			remaining = 0
		}
		value = value[:remaining]
	}
	_, _ = buffer.Buffer.Write(value)
	return originalLength, nil
}
