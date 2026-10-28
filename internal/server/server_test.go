package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/internal/config"
)

func newTestHandler(cfg *config.Config) http.Handler { return NewHandler(cfg, nil) }

func TestListenReportsPortInUse(t *testing.T) {
	taken, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := taken.Close(); err != nil {
			t.Error(err)
		}
	})
	port := taken.Addr().(*net.TCPAddr).Port

	ln, err := Listen(net.IPv4(127, 0, 0, 1), port)
	if err == nil {
		if cerr := ln.Close(); cerr != nil {
			t.Error(cerr)
		}
		t.Fatal("Listen on a taken port succeeded")
	}
	inUse, ok := errors.AsType[*PortInUseError](err)
	if !ok {
		t.Fatalf("err = %v (%T), want *PortInUseError", err, err)
	}
	if !strings.Contains(inUse.Addr, "127.0.0.1:") {
		t.Fatalf("Addr = %q", inUse.Addr)
	}
}

func TestURLs(t *testing.T) {
	got, err := URLs(net.IPv4(127, 0, 0, 1), 8189)
	if err != nil || len(got) != 1 || got[0] != "http://127.0.0.1:8189" {
		t.Fatalf("loopback URLs = %v, %v", got, err)
	}

	got, err = URLs(net.ParseIP("::1"), 8189)
	if err != nil || len(got) != 1 || got[0] != "http://[::1]:8189" {
		t.Fatalf("ipv6 URLs = %v, %v", got, err)
	}

	got, err = URLs(net.IPv4zero, 8189)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "http://127.0.0.1:8189" {
		t.Fatalf("wildcard URLs should lead with loopback, got %v", got)
	}
	for _, u := range got {
		if strings.Contains(u, "[") {
			t.Fatalf("IPv4 wildcard listed an IPv6 URL: %v", got)
		}
	}
}

func TestIsBridge(t *testing.T) {
	for _, name := range []string{"docker0", "br-3f2a9c1d", "veth12ab", "virbr0", "cni0", "podman1"} {
		if !isBridge(name) {
			t.Errorf("isBridge(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"eth0", "enp5s0", "wlan0", "wlp3s0", "tailscale0", "wg0", "tun0", "bond0"} {
		if isBridge(name) {
			t.Errorf("isBridge(%q) = true, want false", name)
		}
	}
}

func TestHealth(t *testing.T) {
	h := newTestHandler(&config.Config{ComfyUI: "http://127.0.0.1:8188"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "healthy" || body["upstream"] != "http://127.0.0.1:8188" {
		t.Fatalf("body = %v", body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v2/health", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	ln, err := Listen(net.IPv4(127, 0, 0, 1), 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, newTestHandler(&config.Config{ComfyUI: "http://x"})) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/api/v2/health")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(shutdownTimeout + time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}
