package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jaideep329/talk-go/disha"
)

var shutdownInitiated atomic.Bool
var exitProcess = os.Exit

func registerCleanupHandlers() {
	log.Println("Registering cleanup handlers...")
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for sig := range signals {
			handleShutdownSignal(sig)
		}
	}()
}

func handleShutdownSignal(sig os.Signal) {
	if !shutdownInitiated.CompareAndSwap(false, true) {
		log.Printf("Received duplicate %s, ignoring (shutdown already in progress)...\n", sig)
		return
	}

	log.Printf("Received %s, checking worker status...\n", sig)
	log.Println("Allowing graceful shutdown to proceed...")

	podName := strings.TrimSpace(os.Getenv("HOSTNAME"))
	if podName == "" {
		log.Println("HOSTNAME is empty; skipping worker graceful shutdown enqueue")
		exitForSignal(sig)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := disha.EnqueueWorkerGracefulShutdown(ctx, dishaDeps, podName); err != nil {
		log.Printf("failed to enqueue graceful shutdown for pod=%s: %v\n", podName, err)
	}
	exitForSignal(sig)
}

func exitForSignal(sig os.Signal) {
	if sig == syscall.SIGINT || sig == os.Interrupt {
		exitProcess(0)
		return
	}
	if sig == syscall.SIGTERM {
		active, reserved := worker.snapshot()
		if active || reserved {
			log.Println("worker is active or reserved; keeping process alive for graceful shutdown")
			return
		}
		exitProcess(0)
	}
}
