// playflow-mock is a deterministic, in-memory API simulator. It never starts
// containers, uses a Docker socket, or contacts PlayFlow.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	settings := faults{
		ReadyDelayMS:      envInt("PLAYFLOW_MOCK_READY_DELAY_MS", 500),
		AmbiguousCreates:  envInt("PLAYFLOW_MOCK_AMBIGUOUS_CREATES", 0),
		RateLimitRequests: envInt("PLAYFLOW_MOCK_RATE_LIMIT_REQUESTS", 0),
		StopFailures:      envInt("PLAYFLOW_MOCK_STOP_FAILURES", 0),
	}
	handler := newMockServer(env("PLAYFLOW_MOCK_API_KEY", "test-playflow-key"), env("PLAYFLOW_MOCK_TEST_MODE", "false") == "true", env("PLAYFLOW_MOCK_PUBLIC_HOST", "127.0.0.1"), settings)
	server := &http.Server{
		Addr: env("PLAYFLOW_MOCK_ADDR", ":8090"), Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("PlayFlow mock listening on %s; test controls enabled=%t", server.Addr, handler.testMode)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < 0 || number > 3600000 {
		log.Fatalf("%s must be an integer from 0 to 3600000", name)
	}
	return number
}
