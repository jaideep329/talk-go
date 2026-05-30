package disha

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jaideep329/talk-go/internal/sentryutil"
)

const (
	workerRegistrationTTL = 24 * time.Hour
	workerSigtermTTL      = 2 * time.Hour
)

type WorkerPodRegistration struct {
	PodIP   string
	PodName string
	PodUID  string
	AppName string
}

func RegisterWorkerPod(ctx context.Context, deps Deps, reg WorkerPodRegistration) error {
	if deps.Redis == nil {
		return errors.New("disha: Redis dependency is required")
	}
	if deps.API == nil {
		return errors.New("disha: API dependency is required")
	}
	if reg.PodIP == "" || reg.PodName == "" || reg.PodUID == "" || reg.AppName == "" {
		return fmt.Errorf("disha: incomplete worker registration: %+v", reg)
	}

	key := workerRegistrationKey(reg.PodName, reg.PodUID)
	if _, ok, err := deps.Redis.GetCache(ctx, key); err != nil {
		return err
	} else if ok {
		if deps.Logger != nil {
			deps.Logger.Printf("disha: worker pod already registered, skipping key=%s\n", key)
		}
		return nil
	}

	// Match Disha's worker registration order: enqueue the DB work first,
	// then write the Redis idempotency key so a failed enqueue can retry.
	if err := deps.API.EnqueueJob(ctx, EnqueueJobRequest{
		ModuleName: "bots.gke_pod_manager",
		FuncName:   "register_worker_pod_db_ops",
		Kwargs: map[string]any{
			"pod_ip":   reg.PodIP,
			"pod_name": reg.PodName,
			"pod_uid":  reg.PodUID,
			"app_name": reg.AppName,
		},
		SQSQueue: "fifo-p0-fast-l1",
	}); err != nil {
		sentryutil.Capture(sentryutil.Event{
			Err: err,
			Tags: map[string]string{
				"component": "worker_lifecycle",
				"operation": "register_worker_pod_db_ops",
			},
			Details: map[string]any{
				"pod_name": reg.PodName,
				"pod_uid":  reg.PodUID,
			},
		})
		return err
	}

	if err := deps.Redis.SetCache(ctx, key, true, workerRegistrationTTL); err != nil {
		sentryutil.Capture(sentryutil.Event{
			Err: err,
			Tags: map[string]string{
				"component": "worker_lifecycle",
				"operation": "set_worker_registration_key",
			},
			Details: map[string]any{
				"pod_name": reg.PodName,
				"pod_uid":  reg.PodUID,
			},
		})
		return err
	}
	return nil
}

func EnqueueWorkerCleanup(ctx context.Context, deps Deps, podName string) error {
	if deps.API == nil {
		return errors.New("disha: API dependency is required")
	}
	if podName == "" {
		return errors.New("disha: pod_name is required")
	}
	if err := deps.API.EnqueueJob(ctx, EnqueueJobRequest{
		ModuleName: "bots.signal_handler",
		FuncName:   "cleanup_state",
		Kwargs: map[string]any{
			"pod_name": podName,
		},
		SQSQueue: "p0-fast-l1",
	}); err != nil {
		sentryutil.Capture(sentryutil.Event{
			Err: err,
			Tags: map[string]string{
				"component": "worker_lifecycle",
				"operation": "cleanup_state",
			},
			Details: map[string]any{
				"pod_name": podName,
			},
		})
		return err
	}
	return nil
}

func EnqueueWorkerGracefulShutdown(ctx context.Context, deps Deps, podName string) error {
	if deps.Redis == nil {
		return errors.New("disha: Redis dependency is required")
	}
	if deps.API == nil {
		return errors.New("disha: API dependency is required")
	}
	if podName == "" {
		return errors.New("disha: pod_name is required")
	}
	if err := deps.Redis.SetCache(ctx, workerSigtermKey(podName), true, workerSigtermTTL); err != nil {
		sentryutil.Capture(sentryutil.Event{
			Err: err,
			Tags: map[string]string{
				"component": "worker_lifecycle",
				"operation": "set_sigterm_key",
			},
			Details: map[string]any{
				"pod_name": podName,
			},
		})
		return err
	}
	if err := deps.API.EnqueueJob(ctx, EnqueueJobRequest{
		ModuleName: "bots.signal_handler",
		FuncName:   "on_graceful_shutdown_initiated",
		Kwargs: map[string]any{
			"pod_name": podName,
		},
		SQSQueue: "fifo-p0-fast-l1",
	}); err != nil {
		sentryutil.Capture(sentryutil.Event{
			Err: err,
			Tags: map[string]string{
				"component": "worker_lifecycle",
				"operation": "on_graceful_shutdown_initiated",
			},
			Details: map[string]any{
				"pod_name": podName,
			},
		})
		return err
	}
	return nil
}

func workerRegistrationKey(podName, podUID string) string {
	return fmt.Sprintf("registered_pod:%s:%s", podName, podUID)
}

func workerSigtermKey(podName string) string {
	return fmt.Sprintf("pod_sigterm:%s", podName)
}
