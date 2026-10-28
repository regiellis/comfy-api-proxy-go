package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/regiellis/comfy-api-proxy-go/internal/config"
	"github.com/regiellis/comfy-api-proxy-go/internal/proxy"
	"github.com/regiellis/comfy-api-proxy-go/internal/server"
	"github.com/regiellis/comfy-api-proxy-go/internal/state"
	"github.com/regiellis/comfy-api-proxy-go/internal/ui"
)

func newRunCmd(stdout io.Writer) *cobra.Command {
	var flags serverFlags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the proxy in the foreground (Ctrl+C to stop)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.load(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return serve(ctx, cfg, stdout)
		},
	}
	flags.bind(cmd.Flags())
	return cmd
}

// portInUse adds where the port value came from and how to change it.
type portInUse struct {
	err    *server.PortInUseError
	source string
}

func (e *portInUse) Error() string {
	return fmt.Sprintf("%v (port from %s).\n"+
		"Another process is holding it. Stop that process, or change the port with "+
		"--port, %sPORT, or port in the config file.\n"+
		"The proxy does not pick another port because clients are configured with this one.",
		e.err, e.source, config.EnvPrefix)
}

func (e *portInUse) Unwrap() error { return e.err }

func serve(ctx context.Context, cfg *config.Config, stdout io.Writer) error {
	ln, err := server.Listen(cfg.BindIP, cfg.Port)
	if err != nil {
		if inUse, ok := errors.AsType[*server.PortInUseError](err); ok {
			return &portInUse{err: inUse, source: cfg.Sources["port"]}
		}
		return err
	}
	urls, err := server.URLs(cfg.BindIP, cfg.Port)
	if err != nil {
		return errors.Join(err, ln.Close())
	}

	var store *state.Store
	var persist proxy.Persist
	if cfg.StateDir != "" {
		store, err = state.Open(cfg.StateDir)
		if err != nil {
			return errors.Join(err, ln.Close())
		}
		persist = store
	}
	out := ui.New(stdout)
	if store != nil {
		for _, w := range store.Warnings {
			out.Warn(w + ". The signing secret in it may be readable by other local users.")
		}
	}
	api, err := proxy.New(cfg, persist, logWriter{out})
	if err != nil {
		return errors.Join(err, ln.Close(), closeStore(store))
	}

	handler := server.NewHandler(cfg, api)
	if !cfg.Quiet {
		handler = server.AccessLog(handler, out.Request)
	}
	out.Startup(banner(cfg, urls))

	cleanupCtx, stopCleanup := context.WithCancel(ctx)
	defer stopCleanup()
	go api.RunCleanup(cleanupCtx)

	serveErr := server.Serve(ctx, ln, handler, api.StopStreams)
	stopCleanup()
	return errors.Join(serveErr, closeStore(store))
}

// logWriter routes the proxy's operational lines through the UI.
type logWriter struct{ u *ui.UI }

func (l logWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if rest, isErr := strings.CutPrefix(msg, "error: "); isErr {
		l.u.Warn(rest)
	} else {
		l.u.Info(msg)
	}
	return len(p), nil
}

func closeStore(s *state.Store) error {
	if s == nil {
		return nil
	}
	return s.Close()
}

func banner(cfg *config.Config, urls []string) ui.Banner {
	file := cfg.File
	if file == "" {
		file = "none"
	}
	auth := "off"
	if cfg.Token != "" {
		auth = "bearer token"
	}
	fields := []ui.Field{
		{Key: "upstream", Value: cfg.ComfyUI},
		{Key: "config", Value: file},
		{Key: "host", Value: cfg.Host, Note: "from " + cfg.Sources["host"]},
		{Key: "auth", Value: auth},
	}
	if cfg.StateDir != "" {
		fields = append(fields, ui.Field{Key: "state", Value: cfg.StateDir})
	}
	if cfg.ComfyUIBaseDir != "" {
		fields = append(fields, ui.Field{Key: "models", Value: cfg.ComfyUIBaseDir, Note: "model uploads on"})
	}
	if len(cfg.CORSOrigins) > 0 {
		fields = append(fields, ui.Field{Key: "cors", Value: strings.Join(cfg.CORSOrigins, ", ")})
	}
	return ui.Banner{Version: version, Fields: fields, URLs: urls}
}
