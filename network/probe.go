package network

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProbeResult is a single, auditable proxy connectivity measurement.
type ProbeResult struct {
	OK         bool          `json:"ok"`
	Target     string        `json:"target"`
	StatusCode int           `json:"status_code,omitempty"`
	LatencyMS  int64         `json:"latency_ms"`
	CheckedAt  time.Time     `json:"checked_at"`
	Error      string        `json:"error,omitempty"`
}

// ProbeHTTP checks whether a proxy can reach an HTTP(S) target.
// It does not log or return proxy credentials. The response body is discarded
// with a strict limit so a health check cannot consume unbounded memory.
func ProbeHTTP(ctx context.Context, proxy ProxyConfig, target string, timeout time.Duration) ProbeResult {
	result := ProbeResult{Target: target, CheckedAt: time.Now().UTC()}
	u, err := url.ParseRequestURI(target)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		result.Error = "target must be an absolute http or https URL"
		return result
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	transport := &http.Transport{
		DialContext:           NewDialer(proxy).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		result.Error = "could not create probe request"
		return result
	}
	started := time.Now()
	resp, err := client.Do(req)
	result.LatencyMS = time.Since(started).Milliseconds()
	result.CheckedAt = time.Now().UTC()
	if err != nil {
		result.Error = safeProbeError(err)
		return result
	}
	defer resp.Body.Close()
	result.StatusCode = resp.StatusCode
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		result.Error = fmt.Sprintf("target returned HTTP %d", resp.StatusCode)
		return result
	}
	result.OK = true
	return result
}

// Keep transport errors useful without exposing URLs that may contain credentials.
func safeProbeError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 240 {
		msg = msg[:240]
	}
	return msg
}
