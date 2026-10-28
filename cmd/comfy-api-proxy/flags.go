package main

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/regiellis/comfy-api-proxy-go/internal/config"
)

// serverFlags are the settings run, start, and config share. Only flags the
// user actually passed become overrides, so env and the config file still
// apply underneath them.
type serverFlags struct {
	configPath                              string
	comfyui, host, token, baseDir, stateDir string
	comfyuiToken                            string
	port, maxUploadMB                       int
	allowInsecureBind, quiet                bool
	corsOrigins                             []string
	publicURL                               string
}

func (s *serverFlags) bind(f *pflag.FlagSet) {
	f.StringVar(&s.configPath, "config", "", "config file (default $XDG_CONFIG_HOME/comfy-api-proxy/config.toml, env "+config.EnvConfig+")")
	f.StringVar(&s.comfyui, "comfyui", config.DefaultComfyUI, "base URL of the self-hosted ComfyUI")
	f.StringVar(&s.host, "host", config.DefaultHost, `bind scope: "local", "global", "localhost", or an IP address`)
	f.IntVar(&s.port, "port", config.DefaultPort, "port to serve the v2 API on (fixed, never auto-selected)")
	f.StringVar(&s.token, "token", "", "require 'Authorization: Bearer <token>' on /api/v2/* requests")
	f.StringVar(&s.comfyuiToken, "comfyui-token", "", "bearer token sent to ComfyUI, when it sits behind an authenticating gateway")
	f.StringVar(&s.baseDir, "comfyui-base-dir", "", "filesystem root of a co-located ComfyUI install (enables model uploads)")
	f.IntVar(&s.maxUploadMB, "max-upload-mb", config.DefaultMaxUploadMB, "max single-request upload size in MB")
	f.BoolVar(&s.allowInsecureBind, "allow-insecure-bind", false, "permit a non-loopback bind without a token (unsafe)")
	f.StringVar(&s.stateDir, "state-dir", "", "directory for proxy-layer SQLite state (opt-in)")
	f.StringVar(&s.publicURL, "public-url", "", "origin clients reach the proxy at, for links in responses (set it behind a TLS-terminating reverse proxy)")
	f.StringArrayVar(&s.corsOrigins, "enable-cors-header", nil, "allow a browser Origin to call this proxy (repeatable, '*' refused)")
	f.BoolVar(&s.quiet, "quiet", false, "turn off the per-request access log")
}

func (s *serverFlags) overrides(cmd *cobra.Command) config.Overrides {
	f := cmd.Flags()
	var o config.Overrides
	if f.Changed("comfyui") {
		o.ComfyUI = &s.comfyui
	}
	if f.Changed("host") {
		o.Host = &s.host
	}
	if f.Changed("port") {
		o.Port = &s.port
	}
	if f.Changed("token") {
		o.Token = &s.token
	}
	if f.Changed("comfyui-token") {
		o.ComfyUIToken = &s.comfyuiToken
	}
	if f.Changed("comfyui-base-dir") {
		o.ComfyUIBaseDir = &s.baseDir
	}
	if f.Changed("max-upload-mb") {
		o.MaxUploadMB = &s.maxUploadMB
	}
	if f.Changed("allow-insecure-bind") {
		o.AllowInsecureBind = &s.allowInsecureBind
	}
	if f.Changed("state-dir") {
		o.StateDir = &s.stateDir
	}
	if f.Changed("public-url") {
		o.PublicURL = &s.publicURL
	}
	if f.Changed("enable-cors-header") {
		o.CORSOrigins = s.corsOrigins
	}
	if f.Changed("quiet") {
		o.Quiet = &s.quiet
	}
	return o
}

func (s *serverFlags) load(cmd *cobra.Command) (*config.Config, error) {
	return config.Load(s.configPath, os.LookupEnv, s.overrides(cmd))
}

// passedArgs rebuilds the flags the user passed, for handing to the
// background child unchanged.
func passedArgs(cmd *cobra.Command) []string {
	var args []string
	cmd.Flags().Visit(func(fl *pflag.Flag) {
		if sv, ok := fl.Value.(pflag.SliceValue); ok {
			for _, v := range sv.GetSlice() {
				args = append(args, "--"+fl.Name+"="+v)
			}
			return
		}
		args = append(args, "--"+fl.Name+"="+fl.Value.String())
	})
	return args
}
