package sidecar

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeStateKeepsLivenessSeparateFromReadiness(t *testing.T) {
	state := &ProbeState{}
	handler := state.Handler()
	assertProbeStatus(t, handler, "/healthz", http.StatusOK)
	assertProbeStatus(t, handler, "/readyz", http.StatusServiceUnavailable)
	state.SetReady(true)
	assertProbeStatus(t, handler, "/readyz", http.StatusOK)
	state.SetReady(false)
	assertProbeStatus(t, handler, "/healthz", http.StatusOK)
}

func assertProbeStatus(t *testing.T, handler http.Handler, path string, expected int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != expected {
		t.Fatalf("probe %s returned %d, want %d", path, response.Code, expected)
	}
}
