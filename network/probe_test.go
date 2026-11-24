package network

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeHTTPDirectSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	got := ProbeHTTP(context.Background(), ProxyConfig{Protocol: ProtoDirect}, server.URL, time.Second)
	if !got.OK || got.StatusCode != http.StatusNoContent {
		t.Fatalf("expected successful probe, got %+v", got)
	}
	if got.LatencyMS < 0 || got.CheckedAt.IsZero() {
		t.Fatalf("invalid measurement: %+v", got)
	}
}

func TestProbeHTTPRejectsInvalidTarget(t *testing.T) {
	got := ProbeHTTP(context.Background(), ProxyConfig{Protocol: ProtoDirect}, "file:///etc/passwd", time.Second)
	if got.OK || got.Error == "" {
		t.Fatalf("expected invalid target to fail, got %+v", got)
	}
}

func TestProbeHTTPReportsTargetFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	got := ProbeHTTP(context.Background(), ProxyConfig{Protocol: ProtoDirect}, server.URL, time.Second)
	if got.OK || got.StatusCode != http.StatusServiceUnavailable || got.Error == "" {
		t.Fatalf("expected target failure, got %+v", got)
	}
}
