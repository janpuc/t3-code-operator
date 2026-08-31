package t3client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
)

type shellSnapshot struct {
	Threads *[]shellThread `json:"threads"`
}

type shellThread struct {
	ID             string `json:"id"`
	ModelSelection struct {
		InstanceID string `json:"instanceId"`
	} `json:"modelSelection"`
	LatestTurn *struct {
		State string `json:"state"`
	} `json:"latestTurn"`
	Session *struct {
		Status             string          `json:"status"`
		ProviderInstanceID string          `json:"providerInstanceId"`
		ActiveTurnID       json.RawMessage `json:"activeTurnId"`
	} `json:"session"`
	HasPendingApprovals bool    `json:"hasPendingApprovals"`
	HasPendingUserInput bool    `json:"hasPendingUserInput"`
	BackgroundLiveness  *string `json:"backgroundLiveness"`
}

func (client *Client) ActiveInstances(ctx context.Context, _ []string) ([]string, error) {
	requestContext, cancel := client.withTimeout(ctx)
	defer cancel()
	token, err := client.bearerToken(requestContext)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(
		requestContext,
		http.MethodGet,
		client.endpoint("/api/orchestration/shell").String(),
		nil,
	)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.httpClient.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, errors.New("read t3 orchestration activity")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read t3 orchestration activity: HTTP status %d", response.StatusCode)
	}
	raw, err := readBoundedResponse(response.Body, maxHTTPResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("read t3 orchestration activity: %w", err)
	}
	var snapshot shellSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.Threads == nil {
		return nil, errors.New("t3 orchestration activity response is invalid")
	}
	active := map[string]struct{}{}
	for _, thread := range *snapshot.Threads {
		if !threadIsActive(thread) {
			continue
		}
		found := false
		for _, instanceID := range []string{
			thread.ModelSelection.InstanceID,
			providerInstanceID(thread),
		} {
			if instanceID == "" {
				continue
			}
			active[instanceID] = struct{}{}
			found = true
		}
		if !found {
			active["unknown"] = struct{}{}
		}
	}
	result := make([]string, 0, len(active))
	for instanceID := range active {
		result = append(result, instanceID)
	}
	sort.Strings(result)
	return result, nil
}

func providerInstanceID(thread shellThread) string {
	if thread.Session == nil {
		return ""
	}
	return thread.Session.ProviderInstanceID
}

func threadIsActive(thread shellThread) bool {
	if thread.HasPendingApprovals || thread.HasPendingUserInput || thread.BackgroundLiveness != nil {
		return true
	}
	if thread.LatestTurn != nil {
		switch thread.LatestTurn.State {
		case "completed", "interrupted", "error":
		case "running":
			return true
		default:
			return true
		}
	}
	if thread.Session == nil {
		return false
	}
	if rawJSONIsNonNull(thread.Session.ActiveTurnID) {
		return true
	}
	switch thread.Session.Status {
	case "idle", "ready", "interrupted", "stopped", "error":
		return false
	case "starting", "running":
		return true
	default:
		return true
	}
}

func rawJSONIsNonNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null"))
}
