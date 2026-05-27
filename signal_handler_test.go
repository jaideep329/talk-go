package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jaideep329/talk-go/disha"
)

type signalAPIRecorder struct {
	mu       sync.Mutex
	requests []map[string]any
}

func newSignalAPIServer(t *testing.T) (*httptest.Server, *signalAPIRecorder) {
	t.Helper()
	recorder := &signalAPIRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("Unmarshal request: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		recorder.mu.Lock()
		recorder.requests = append(recorder.requests, body)
		recorder.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(server.Close)
	return server, recorder
}

func (r *signalAPIRecorder) snapshot() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, len(r.requests))
	copy(out, r.requests)
	return out
}

func TestHandleShutdownSignalEnqueuesGracefulShutdownOnce(t *testing.T) {
	shutdownInitiated.Store(false)
	exitCodes := []int{}
	previousExitProcess := exitProcess
	exitProcess = func(code int) {
		exitCodes = append(exitCodes, code)
	}
	t.Cleanup(func() {
		shutdownInitiated.Store(false)
		exitProcess = previousExitProcess
		worker.finish()
	})

	redisServer := miniredis.RunT(t)
	redisClient := disha.NewRedisClient(redisServer.Addr(), "", 0, nil)
	t.Cleanup(func() {
		if err := redisClient.Close(); err != nil {
			t.Fatalf("Close redis: %v", err)
		}
		redisServer.Close()
	})

	apiServer, apiRecorder := newSignalAPIServer(t)
	previousDeps := dishaDeps
	dishaDeps = disha.Deps{
		Redis: redisClient,
		API:   disha.NewAPIClient(apiServer.URL, time.Second, nil),
	}
	t.Cleanup(func() {
		dishaDeps = previousDeps
	})
	t.Setenv("HOSTNAME", "pod-1")

	handleShutdownSignal(syscall.SIGTERM)
	handleShutdownSignal(syscall.SIGTERM)

	raw, ok, err := redisClient.GetCache(context.Background(), "pod_sigterm:pod-1")
	if err != nil {
		t.Fatalf("GetCache sigterm key: %v", err)
	}
	if !ok || string(raw) != "true" {
		t.Fatalf("sigterm cache = %q (present=%v), want JSON true", raw, ok)
	}

	requests := apiRecorder.snapshot()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1: %+v", len(requests), requests)
	}
	kwargs, ok := requests[0]["kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("kwargs = %#v, want object", requests[0]["kwargs"])
	}
	if requests[0]["module_name"] != "bots.signal_handler" ||
		requests[0]["func_name"] != "on_graceful_shutdown_initiated" ||
		requests[0]["sqs_queue"] != "fifo-p0-fast-l1" ||
		kwargs["pod_name"] != "pod-1" {
		t.Fatalf("enqueue body mismatch: %+v", requests[0])
	}
	if len(exitCodes) != 1 || exitCodes[0] != 0 {
		t.Fatalf("exit codes = %+v, want [0]", exitCodes)
	}
}

func TestHandleShutdownSignalKeepsActiveWorkerAlive(t *testing.T) {
	shutdownInitiated.Store(false)
	if !worker.tryStart() {
		t.Fatal("worker should start from idle state")
	}
	exitCodes := []int{}
	previousExitProcess := exitProcess
	exitProcess = func(code int) {
		exitCodes = append(exitCodes, code)
	}
	t.Cleanup(func() {
		shutdownInitiated.Store(false)
		exitProcess = previousExitProcess
		worker.finish()
	})

	redisServer := miniredis.RunT(t)
	redisClient := disha.NewRedisClient(redisServer.Addr(), "", 0, nil)
	t.Cleanup(func() {
		if err := redisClient.Close(); err != nil {
			t.Fatalf("Close redis: %v", err)
		}
		redisServer.Close()
	})

	apiServer, apiRecorder := newSignalAPIServer(t)
	previousDeps := dishaDeps
	dishaDeps = disha.Deps{
		Redis: redisClient,
		API:   disha.NewAPIClient(apiServer.URL, time.Second, nil),
	}
	t.Cleanup(func() {
		dishaDeps = previousDeps
	})
	t.Setenv("HOSTNAME", "pod-1")

	handleShutdownSignal(syscall.SIGTERM)

	if len(apiRecorder.snapshot()) != 1 {
		t.Fatalf("request count = %d, want 1", len(apiRecorder.snapshot()))
	}
	if len(exitCodes) != 0 {
		t.Fatalf("exit codes = %+v, want none for active worker", exitCodes)
	}
}
