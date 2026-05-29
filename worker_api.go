package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jaideep329/talk-go/disha"
	"github.com/jaideep329/talk-go/voicepipelinecore"
)

type workerRuntime struct {
	mu       sync.Mutex
	active   bool
	reserved bool
	task     *voicepipelinecore.PipelineTask
}

func (w *workerRuntime) tryStart() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active {
		return false
	}
	w.active = true
	return true
}

func (w *workerRuntime) setTask(task *voicepipelinecore.PipelineTask) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.task = task
}

func (w *workerRuntime) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.active = false
	w.reserved = false
	w.task = nil
}

func (w *workerRuntime) markReserved() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reserved = true
}

func (w *workerRuntime) snapshot() (active, reserved bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active, w.reserved
}

type workerRoomRequest struct {
	RoomURL        string `json:"room_url"`
	BotToken       string `json:"bot_token"`
	Token          string `json:"token"`
	ConversationID string `json:"conversation_id"`
	BotWorkerType  string `json:"bot_worker_type"`
}

func handleCreateWorkerRoom(w http.ResponseWriter, r *http.Request) {
	var req workerRoomRequest
	if !decodeJSONRequest(w, r, &req) {
		return
	}
	if !worker.tryStart() {
		http.Error(w, "Worker machine already has an active session", http.StatusConflict)
		return
	}

	go runWorkerRoom(req)
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "success",
		"room_url": req.RoomURL,
	})
}

func (r *workerRoomRequest) normalize() {
	r.RoomURL = strings.TrimSpace(r.RoomURL)
	r.BotToken = strings.TrimSpace(r.BotToken)
	r.Token = strings.TrimSpace(r.Token)
	r.ConversationID = strings.TrimSpace(r.ConversationID)
	r.BotWorkerType = strings.TrimSpace(r.BotWorkerType)
	if r.BotToken == "" {
		r.BotToken = r.Token
	}
}

func (r workerRoomRequest) validate() error {
	return requireFields(
		requiredField{Name: "room_url", Value: r.RoomURL},
		requiredField{Name: "token", Value: r.Token},
		requiredField{Name: "conversation_id", Value: r.ConversationID},
		requiredField{Name: "bot_worker_type", Value: r.BotWorkerType},
	)
}

func (r workerRoomRequest) connectRequest() connectRequest {
	return connectRequest{
		ConversationID: r.ConversationID,
		BotType:        r.BotWorkerType,
		RoomURL:        r.RoomURL,
		Token:          r.Token,
		BotToken:       r.BotToken,
	}
}

type missingFieldsError struct {
	fields []string
}

func (e *missingFieldsError) Error() string {
	return "Missing required parameters: " + strings.Join(e.fields, ", ")
}

func runWorkerRoom(req workerRoomRequest) {
	// Pin the pod against autoscaler eviction for the lifetime of the
	// call. This mirrors Python `BotWorkerManager.create_bot_task`
	// which calls `set_safe_to_evict(pod_name, safe_to_evict=False)`
	// right before launching the bot. It's idempotent with the pin done
	// at reservation time and covers the direct `/connect` path, which
	// never goes through mark_machine_reserved.
	pinPodAgainstEviction()

	task, err := prepareTask(context.Background(), req.connectRequest(), func(*voicepipelinecore.PipelineTask) {
		unpinPodAfterCall()
		finishWorkerAndQueueCleanup()
	})
	if err != nil {
		log.Printf("worker task failed to start conversation=%s: %v\n", req.ConversationID, err)
		unpinPodAfterCall()
		finishWorkerAndQueueCleanup()
		return
	}
	worker.setTask(task)
	log.Printf("worker task started conversation=%s room=%s\n", req.ConversationID, task.RoomName)
	task.Start()
}

// pinPodAgainstEviction best-effort sets the GKE safe-to-evict
// annotation to "false" so the cluster autoscaler can't evict the pod
// while it is reserved or running a call. Logs failures and otherwise
// no-ops outside Kubernetes.
func pinPodAgainstEviction() {
	if dishaDeps.GKEPatcher == nil {
		return
	}
	podName := strings.TrimSpace(os.Getenv("HOSTNAME"))
	if podName == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dishaDeps.GKEPatcher.SetSafeToEvict(ctx, podName, false); err != nil {
		log.Printf("failed to set safe-to-evict=false on pod=%s: %v\n", podName, err)
	}
}

// unpinPodAfterCall restores safe-to-evict so the pod can be reaped
// after Disha's cleanup_state job tears it down. Failures are logged
// but never block the cleanup path.
func unpinPodAfterCall() {
	if dishaDeps.GKEPatcher == nil {
		return
	}
	podName := strings.TrimSpace(os.Getenv("HOSTNAME"))
	if podName == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dishaDeps.GKEPatcher.SetSafeToEvict(ctx, podName, true); err != nil {
		log.Printf("failed to set safe-to-evict=true on pod=%s: %v\n", podName, err)
	}
}

func finishWorkerAndQueueCleanup() {
	worker.finish()
	podName := strings.TrimSpace(os.Getenv("HOSTNAME"))
	if podName == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := disha.EnqueueWorkerCleanup(ctx, dishaDeps, podName); err != nil {
		log.Printf("failed to enqueue worker cleanup for pod=%s: %v\n", podName, err)
	}
}

func handleHasActiveSession(w http.ResponseWriter, r *http.Request) {
	active, _ := worker.snapshot()
	activeSessions := 0
	if active {
		activeSessions = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"has_active_session": active,
		"active_sessions":    activeSessions,
	})
}

func handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
}

func handleReadinessCheck(w http.ResponseWriter, r *http.Request) {
	active, reserved := worker.snapshot()
	if active || reserved {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not ready",
			"detail": "Worker is active or reserved",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func handleMarkMachineReserved(w http.ResponseWriter, r *http.Request) {
	worker.markReserved()
	// Pin the pod as soon as it is reserved — Disha reserves several
	// seconds before the user joins, and an autoscaler scale-down in
	// that window would kill the pod before the call starts. Mirrors
	// Python `mark_machine_reserved`, which also flips safe-to-evict to
	// false. The matching unpin happens on call cleanup.
	pinPodAgainstEviction()
	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

func handleTriggerExit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
	go func() {
		time.Sleep(50 * time.Millisecond)
		os.Exit(0)
	}()
}

type validatedJSONRequest interface {
	normalize()
	validate() error
}

type requiredField struct {
	Name  string
	Value string
}

func requireMethod(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

func decodeJSONRequest(w http.ResponseWriter, r *http.Request, req validatedJSONRequest) bool {
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	req.normalize()
	if err := req.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func requireFields(fields ...requiredField) error {
	missing := make([]string, 0, len(fields))
	for _, field := range fields {
		if strings.TrimSpace(field.Value) == "" {
			missing = append(missing, field.Name)
		}
	}
	if len(missing) > 0 {
		return &missingFieldsError{fields: missing}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("failed to write JSON response: %v\n", err)
	}
}
