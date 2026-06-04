package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func resetWorkerForTest(t *testing.T) {
	t.Helper()
	worker.finish()
	t.Cleanup(func() {
		worker.finish()
	})
}

func TestWorkerPodRegistrationFromEnv(t *testing.T) {
	t.Setenv("HOSTNAME", "pod-1")
	t.Setenv("POD_UID", "uid-1")
	t.Setenv("GKE_DEPLOYMENT_NAME", "sales-worker")
	t.Setenv("POD_IP", "10.1.2.3")

	reg, ok, err := workerPodRegistrationFromEnv()
	if err != nil {
		t.Fatalf("workerPodRegistrationFromEnv: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if reg.PodName != "pod-1" || reg.PodUID != "uid-1" || reg.AppName != "sales-worker" || reg.PodIP != "10.1.2.3" {
		t.Fatalf("registration mismatch: %+v", reg)
	}
}

func TestWorkerPodRegistrationFromEnvSkipsLocalWhenIncomplete(t *testing.T) {
	t.Setenv("HOSTNAME", "pod-1")
	t.Setenv("POD_UID", "")
	t.Setenv("GKE_DEPLOYMENT_NAME", "sales-worker")
	t.Setenv("POD_IP", "10.1.2.3")

	_, ok, err := workerPodRegistrationFromEnv()
	if err != nil {
		t.Fatalf("workerPodRegistrationFromEnv: %v", err)
	}
	if ok {
		t.Fatal("ok = true, want false")
	}
}

func TestHandleHasActiveSession(t *testing.T) {
	resetWorkerForTest(t)
	if !worker.tryStart() {
		t.Fatal("tryStart returned false")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bot/has_active_session", nil)
	handleHasActiveSession(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	if body["has_active_session"] != true || body["active_sessions"] != float64(1) {
		t.Fatalf("body mismatch: %+v", body)
	}
}

func TestHandleReadinessCheck(t *testing.T) {
	resetWorkerForTest(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bot/readiness_check", nil)
	handleReadinessCheck(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want %d", rec.Code, http.StatusOK)
	}

	worker.markReserved()
	rec = httptest.NewRecorder()
	handleReadinessCheck(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("reserved status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), "Worker is active or reserved") {
		t.Fatalf("body = %q, want reserved detail", rec.Body.String())
	}
}

func TestHandleCreateWorkerRoomRejectsMissingFields(t *testing.T) {
	resetWorkerForTest(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/bot/create_worker_room", strings.NewReader(`{}`))
	handleCreateWorkerRoom(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	active, _ := worker.snapshot()
	if active {
		t.Fatal("worker active = true, want false")
	}
}

func TestHandleCreateWorkerRoomReturnsConflictWhenActive(t *testing.T) {
	resetWorkerForTest(t)
	if !worker.tryStart() {
		t.Fatal("tryStart returned false")
	}

	body := `{"room_url":"https://room.daily.co/test","room_name":"test","token":"user-token","conversation_id":"conv-1","bot_worker_type":"sales_call"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/bot/create_worker_room", strings.NewReader(body))
	handleCreateWorkerRoom(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if !strings.Contains(rec.Body.String(), "Worker machine already has an active session") {
		t.Fatalf("body = %q, want conflict detail", rec.Body.String())
	}
}
