package sidecar

import (
	"net/http"
	"sync/atomic"
)

type ProbeState struct {
	ready atomic.Bool
}

func (state *ProbeState) SetReady(ready bool) {
	if state == nil {
		return
	}
	state.ready.Store(ready)
}

func (state *ProbeState) Ready() bool {
	return state != nil && state.ready.Load()
}

func (state *ProbeState) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !state.Ready() {
			response.WriteHeader(http.StatusServiceUnavailable)
			_, _ = response.Write([]byte("not ready\n"))
			return
		}
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("ready\n"))
	})
	return mux
}
