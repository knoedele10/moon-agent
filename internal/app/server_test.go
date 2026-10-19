package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestShutdownCancelsStreamingRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestStopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: ready\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(requestStopped)
		}))
	}()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	cancel()
	select {
	case <-requestStopped:
	case <-time.After(3 * time.Second):
		t.Fatal("streaming handler did not stop")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP server did not shut down")
	}
}

func TestMissingSecretFailsBeforeOpeningDatabase(t *testing.T) {
	t.Setenv("APP_SECRET", "")
	if err := Run(context.Background()); err == nil || !strings.Contains(err.Error(), "APP_SECRET") {
		t.Fatalf("startup result: %v", err)
	}
}
