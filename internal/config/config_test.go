package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// isolate points the default config location at an empty directory so a real
// ~/.config/comfy-api-proxy/config.toml cannot leak into a test.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func mustLoad(t *testing.T, configFlag string, vars map[string]string, flags Overrides) *Config {
	t.Helper()
	cfg, err := Load(configFlag, env(vars), flags)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func loadErr(t *testing.T, configFlag string, vars map[string]string, flags Overrides) string {
	t.Helper()
	_, err := Load(configFlag, env(vars), flags)
	if err == nil {
		t.Fatal("Load succeeded, want error")
	}
	if _, ok := errors.AsType[*Error](err); !ok {
		t.Fatalf("error %v is %T, want *config.Error", err, err)
	}
	return err.Error()
}

func TestDefaults(t *testing.T) {
	isolate(t)
	cfg := mustLoad(t, "", nil, Overrides{})
	if cfg.Port != 8189 || cfg.Host != "local" || cfg.ComfyUI != DefaultComfyUI || cfg.MaxUploadMB != 100 {
		t.Fatalf("defaults = %+v", cfg)
	}
	if got := cfg.BindIP.String(); got != "127.0.0.1" {
		t.Fatalf("BindIP = %s, want 127.0.0.1", got)
	}
	if cfg.File != "" {
		t.Fatalf("File = %q, want none", cfg.File)
	}
	if cfg.Sources["port"] != "default" {
		t.Fatalf("port source = %q", cfg.Sources["port"])
	}
}

func TestPrecedenceFlagOverEnvOverFile(t *testing.T) {
	isolate(t)
	path := writeConfig(t, "port = 9000\nhost = \"global\"\ntoken = \"file-token\"\n")

	cfg := mustLoad(t, path, nil, Overrides{})
	if cfg.Port != 9000 || cfg.Sources["port"] != "config "+path {
		t.Fatalf("file layer: port=%d source=%q", cfg.Port, cfg.Sources["port"])
	}
	if cfg.BindIP.String() != "0.0.0.0" {
		t.Fatalf("file layer: BindIP=%s", cfg.BindIP)
	}

	vars := map[string]string{"COMFY_PROXY_PORT": "9100"}
	cfg = mustLoad(t, path, vars, Overrides{})
	if cfg.Port != 9100 || cfg.Sources["port"] != "env COMFY_PROXY_PORT" {
		t.Fatalf("env layer: port=%d source=%q", cfg.Port, cfg.Sources["port"])
	}

	cfg = mustLoad(t, path, vars, Overrides{Port: new(9200)})
	if cfg.Port != 9200 || cfg.Sources["port"] != "flag --port" {
		t.Fatalf("flag layer: port=%d source=%q", cfg.Port, cfg.Sources["port"])
	}
	if cfg.Token != "file-token" {
		t.Fatalf("token from file lost: %q", cfg.Token)
	}
}

func TestDefaultConfigLocation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "comfy-api-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "comfy-api-proxy", "config.toml")
	if err := os.WriteFile(path, []byte("port = 9300\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, "", nil, Overrides{})
	if cfg.Port != 9300 || cfg.File != path {
		t.Fatalf("port=%d file=%q", cfg.Port, cfg.File)
	}
}

func TestEnvConfigPath(t *testing.T) {
	isolate(t)
	path := writeConfig(t, "port = 9400\n")
	cfg := mustLoad(t, "", map[string]string{EnvConfig: path}, Overrides{})
	if cfg.Port != 9400 {
		t.Fatalf("port = %d", cfg.Port)
	}
}

func TestExplicitConfigMustExist(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "nope.toml")
	if msg := loadErr(t, missing, nil, Overrides{}); !strings.Contains(msg, "cannot read config file") {
		t.Fatalf("flag path: %s", msg)
	}
	if msg := loadErr(t, "", map[string]string{EnvConfig: missing}, Overrides{}); !strings.Contains(msg, "cannot read config file") {
		t.Fatalf("env path: %s", msg)
	}
}

func TestUnknownKeyFails(t *testing.T) {
	isolate(t)
	path := writeConfig(t, "bnd = \"global\"\n")
	if msg := loadErr(t, path, nil, Overrides{}); !strings.Contains(msg, "unknown keys") || !strings.Contains(msg, "bnd") {
		t.Fatalf("msg = %s", msg)
	}
}

func TestMalformedFileFails(t *testing.T) {
	isolate(t)
	path := writeConfig(t, "port = \n")
	if msg := loadErr(t, path, nil, Overrides{}); !strings.Contains(msg, path) {
		t.Fatalf("msg = %s", msg)
	}
}

func TestBadEnvValues(t *testing.T) {
	isolate(t)
	cases := map[string]string{
		"COMFY_PROXY_PORT":                "abc",
		"COMFY_PROXY_MAX_UPLOAD_MB":       "lots",
		"COMFY_PROXY_ALLOW_INSECURE_BIND": "maybe",
	}
	for k, v := range cases {
		if msg := loadErr(t, "", map[string]string{k: v}, Overrides{}); !strings.Contains(msg, k) {
			t.Errorf("%s=%s: msg %q does not name the variable", k, v, msg)
		}
	}
}

func TestEnvCORSOriginsSplit(t *testing.T) {
	isolate(t)
	vars := map[string]string{"COMFY_PROXY_CORS_ORIGINS": "https://a.example, http://b.example:8080/ ,"}
	cfg := mustLoad(t, "", vars, Overrides{})
	want := []string{"https://a.example", "http://b.example:8080"}
	if strings.Join(cfg.CORSOrigins, " ") != strings.Join(want, " ") {
		t.Fatalf("origins = %v, want %v", cfg.CORSOrigins, want)
	}
}

func TestBindGate(t *testing.T) {
	isolate(t)
	cases := []struct {
		name  string
		flags Overrides
		ok    bool
	}{
		{"local needs nothing", Overrides{Host: new("local")}, true},
		{"localhost is loopback", Overrides{Host: new("localhost")}, true},
		{"ipv6 loopback", Overrides{Host: new("::1")}, true},
		{"global refused without token", Overrides{Host: new("global")}, false},
		{"explicit lan ip refused", Overrides{Host: new("192.168.1.5")}, false},
		{"global with token", Overrides{Host: new("global"), Token: new("s3cret")}, true},
		{"global with insecure opt-out", Overrides{Host: new("global"), AllowInsecureBind: new(true)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load("", env(nil), tc.flags)
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "refusing to bind")) {
				t.Fatalf("err = %v, want refusing to bind", err)
			}
		})
	}
}

func TestRefusalNamesSourceAndFixes(t *testing.T) {
	isolate(t)
	msg := loadErr(t, "", map[string]string{"COMFY_PROXY_HOST": "global"}, Overrides{})
	for _, want := range []string{"env COMFY_PROXY_HOST", "--token", "COMFY_PROXY_TOKEN", "allow_insecure_bind"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q missing %q", msg, want)
		}
	}
}

func TestResolveHost(t *testing.T) {
	cases := map[string]string{
		"local":       "127.0.0.1",
		"localhost":   "127.0.0.1",
		"global":      "0.0.0.0",
		"10.0.0.7":    "10.0.0.7",
		"::":          "::",
		"[::1]":       "::1",
		" 127.0.0.2 ": "127.0.0.2",
	}
	for in, want := range cases {
		ip, err := ResolveHost(in)
		if err != nil || ip.String() != want {
			t.Errorf("ResolveHost(%q) = %v, %v; want %s", in, ip, err, want)
		}
	}
	for _, bad := range []string{"", "   ", "example.com", "300.1.1.1"} {
		if _, err := ResolveHost(bad); err == nil {
			t.Errorf("ResolveHost(%q) succeeded, want error", bad)
		}
	}
}

func TestValidation(t *testing.T) {
	isolate(t)
	cases := []struct {
		flags Overrides
		want  string
	}{
		{Overrides{Port: new(0)}, "port must be between"},
		{Overrides{Port: new(70000)}, "port must be between"},
		{Overrides{MaxUploadMB: new(0)}, "max_upload_mb"},
		{Overrides{ComfyUI: new("ftp://host")}, "comfyui must be"},
		{Overrides{ComfyUI: new("http://")}, "comfyui must be"},
		{Overrides{CORSOrigins: []string{"*"}}, "invalid --enable-cors-header"},
	}
	for _, tc := range cases {
		if msg := loadErr(t, "", nil, tc.flags); !strings.Contains(msg, tc.want) {
			t.Errorf("msg %q, want %q", msg, tc.want)
		}
	}
}

func TestComfyUITrailingSlashTrimmed(t *testing.T) {
	isolate(t)
	cfg := mustLoad(t, "", nil, Overrides{ComfyUI: new("http://gpu-box:8188/")})
	if cfg.ComfyUI != "http://gpu-box:8188" {
		t.Fatalf("ComfyUI = %q", cfg.ComfyUI)
	}
}

func TestComfyUITokenLayers(t *testing.T) {
	isolate(t)
	file := writeConfig(t, `comfyui_token = "from-file"`)
	if cfg := mustLoad(t, file, nil, Overrides{}); cfg.ComfyUIToken != "from-file" {
		t.Fatalf("file token = %q", cfg.ComfyUIToken)
	}
	cfg := mustLoad(t, file, map[string]string{"COMFY_PROXY_COMFYUI_TOKEN": "from-env"}, Overrides{})
	if cfg.ComfyUIToken != "from-env" || cfg.Sources["comfyui_token"] != "env COMFY_PROXY_COMFYUI_TOKEN" {
		t.Fatalf("env token = %q from %q", cfg.ComfyUIToken, cfg.Sources["comfyui_token"])
	}
	if cfg := mustLoad(t, file, nil, Overrides{ComfyUIToken: new("from-flag")}); cfg.ComfyUIToken != "from-flag" {
		t.Fatalf("flag token = %q", cfg.ComfyUIToken)
	}
}

func TestPublicURL(t *testing.T) {
	isolate(t)
	if cfg := mustLoad(t, "", nil, Overrides{}); cfg.PublicURL != "" {
		t.Fatalf("default public_url = %q, want unset", cfg.PublicURL)
	}
	file := writeConfig(t, `public_url = "HTTPS://Proxy.Example.com/"`)
	if cfg := mustLoad(t, file, nil, Overrides{}); cfg.PublicURL != "https://proxy.example.com" {
		t.Fatalf("file public_url = %q", cfg.PublicURL)
	}
	cfg := mustLoad(t, file, map[string]string{"COMFY_PROXY_PUBLIC_URL": "https://gpu.example.net/comfy/"}, Overrides{})
	if cfg.PublicURL != "https://gpu.example.net/comfy" || cfg.Sources["public_url"] != "env COMFY_PROXY_PUBLIC_URL" {
		t.Fatalf("env public_url = %q from %q", cfg.PublicURL, cfg.Sources["public_url"])
	}
	if cfg := mustLoad(t, file, nil, Overrides{PublicURL: new("http://10.0.0.5:8189")}); cfg.PublicURL != "http://10.0.0.5:8189" {
		t.Fatalf("flag public_url = %q", cfg.PublicURL)
	}
	for _, bad := range []string{"proxy.example.com", "ftp://proxy.example.com", "https://", "https://u:p@proxy.example.com", "https://proxy.example.com/?x=1", "https://proxy.example.com/#f"} {
		if msg := loadErr(t, "", nil, Overrides{PublicURL: new(bad)}); !strings.Contains(msg, "public_url") {
			t.Errorf("public_url %q: error %q does not name the key", bad, msg)
		}
	}
}
