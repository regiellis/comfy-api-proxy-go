package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/internal/config"
)

const shutdownTimeout = 5 * time.Second

// Routes registers the v2 surface on a mux.
type Routes interface {
	Register(mux *http.ServeMux)
}

// NewHandler builds the /api/v2 router wrapped in the middleware chain. api
// may be nil, which serves health alone.
func NewHandler(cfg *config.Config, api Routes) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v2/health", health(cfg.ComfyUI))
	if api != nil {
		api.Register(mux)
	}

	allowed := originSet(cfg.CORSOrigins)
	var h http.Handler = mux
	if cfg.Token != "" {
		h = bearerAuth(cfg.Token, h)
	}
	h = originGuard(allowed, h)
	if len(allowed) > 0 {
		h = cors(allowed, h)
	}
	return h
}

// health is a process probe. It never calls ComfyUI and stays reachable
// without a token so a supervisor can check liveness.
func health(upstream string) http.HandlerFunc {
	body := struct {
		Status   string `json:"status"`
		Upstream string `json:"upstream"`
	}{"healthy", upstream}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			return // client went away; nothing left to tell it
		}
	}
}

// Serve runs handler on ln until ctx is cancelled, then drains in-flight
// requests for up to shutdownTimeout. onShutdown hooks run as shutdown
// starts; long-lived responses such as SSE streams use them to end, since
// they would otherwise hold the drain open until the timeout.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler, onShutdown ...func()) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	for _, f := range onShutdown {
		srv.RegisterOnShutdown(f)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
