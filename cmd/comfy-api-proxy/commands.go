package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/regiellis/comfy-api-proxy-go/internal/config"
	"github.com/regiellis/comfy-api-proxy-go/internal/service"
	"github.com/regiellis/comfy-api-proxy-go/internal/ui"
)

func newStartCmd(stdout io.Writer) *cobra.Command {
	var flags serverFlags
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the proxy in the background",
		Long: "Start the proxy in the background. It takes the same flags as run, " +
			"logs to the state directory, and is managed with stop and status.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Validate here, in the foreground, so a refused bind or a bad
			// config file fails where the user can see it.
			cfg, err := flags.load(cmd)
			if err != nil {
				return err
			}
			rec, logPath, err := service.Start(passedArgs(cmd), cfg.Host, cfg.BindIP, cfg.Port)
			if err != nil {
				return err
			}
			out := ui.New(stdout)
			out.Success(fmt.Sprintf("comfy-api-proxy started at %s (pid %d)", rec.URL, rec.PID))
			out.Fields([]ui.Field{
				{Key: "logs", Value: logPath},
				{Key: "stop", Value: "comfy-api-proxy stop"},
			})
			return nil
		},
	}
	flags.bind(cmd.Flags())
	return cmd
}

func newStopCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background proxy",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			pid, err := service.Stop()
			if err != nil {
				return err
			}
			out := ui.New(stdout)
			if pid == 0 {
				out.Info("comfy-api-proxy is not running.")
				return nil
			}
			out.Success(fmt.Sprintf("comfy-api-proxy stopped (pid %d).", pid))
			return nil
		},
	}
}

func newStatusCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the background proxy is running",
		Long:  "Show whether the background proxy is running. Exits 0 when it is and 1 when it is not.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rec, err := service.Running()
			if err != nil {
				return err
			}
			out := ui.New(stdout)
			if rec == nil {
				out.Info("comfy-api-proxy is not running.")
				return &errSilent{code: exitRuntime}
			}
			out.Success(fmt.Sprintf("comfy-api-proxy is running at %s (pid %d).", rec.URL, rec.PID))
			fields := []ui.Field{}
			if h, err := service.Probe(cmd.Context(), rec.URL); err != nil {
				fields = append(fields, ui.Field{Key: "health", Value: "no answer", Note: err.Error()})
			} else {
				fields = append(fields, ui.Field{Key: "health", Value: h.Status}, ui.Field{Key: "upstream", Value: h.Upstream})
			}
			if logPath, err := service.LogPath(); err == nil {
				fields = append(fields, ui.Field{Key: "logs", Value: logPath})
			}
			out.Fields(fields)
			return nil
		},
	}
}

// newConfigCmd prints the resolved configuration and where each value came
// from, which is the first question when a remote bind does not answer.
func newConfigCmd(stdout io.Writer) *cobra.Command {
	var flags serverFlags
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show the resolved configuration and where each value came from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.load(cmd)
			if err != nil {
				return err
			}
			out := ui.New(stdout)
			path, explicit := config.Path(flags.configPath, os.LookupEnv)
			fileNote := "not found, using defaults"
			if cfg.File != "" {
				fileNote = "loaded"
			} else if path == "" {
				fileNote = "no default location"
			}
			if explicit {
				fileNote += ", named explicitly"
			}
			out.Fields([]ui.Field{{Key: "file", Value: orNone(path), Note: fileNote}})
			out.Fields(settingFields(cfg))
			return nil
		},
	}
	flags.bind(cmd.Flags())
	return cmd
}

func settingFields(cfg *config.Config) []ui.Field {
	src := func(key string) string {
		if s, ok := cfg.Sources[key]; ok {
			return s
		}
		return "default"
	}
	token := "unset"
	if cfg.Token != "" {
		token = "set (" + strconv.Itoa(len(cfg.Token)) + " characters)"
	}
	comfyToken := "unset"
	if cfg.ComfyUIToken != "" {
		comfyToken = "set (" + strconv.Itoa(len(cfg.ComfyUIToken)) + " characters)"
	}
	return []ui.Field{
		{Key: "comfyui", Value: cfg.ComfyUI, Note: src("comfyui")},
		{Key: "host", Value: cfg.Host + " → " + cfg.BindIP.String(), Note: src("host")},
		{Key: "port", Value: strconv.Itoa(cfg.Port), Note: src("port")},
		{Key: "token", Value: token, Note: src("token")},
		{Key: "comfyui token", Value: comfyToken, Note: src("comfyui_token")},
		{Key: "insecure", Value: strconv.FormatBool(cfg.AllowInsecureBind), Note: src("allow_insecure_bind")},
		{Key: "base dir", Value: orNone(cfg.ComfyUIBaseDir), Note: src("comfyui_base_dir")},
		{Key: "upload", Value: strconv.Itoa(cfg.MaxUploadMB) + " MB", Note: src("max_upload_mb")},
		{Key: "state", Value: orNone(cfg.StateDir), Note: src("state_dir")},
		{Key: "cors", Value: orNone(strings.Join(cfg.CORSOrigins, ", ")), Note: src("cors_origins")},
		{Key: "public url", Value: orNone(cfg.PublicURL), Note: src("public_url")},
		{Key: "quiet", Value: strconv.FormatBool(cfg.Quiet), Note: src("quiet")},
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
