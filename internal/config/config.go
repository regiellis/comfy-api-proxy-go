// Package config resolves the proxy's settings from defaults, a TOML file,
// COMFY_PROXY_* environment variables, and flags, in that order of precedence.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	DefaultComfyUI     = "http://127.0.0.1:8188"
	DefaultHost        = HostLocal
	DefaultPort        = 8189
	DefaultMaxUploadMB = 100

	// HostLocal and HostGlobal are the named bind scopes accepted wherever a
	// host is, alongside a literal IP or "localhost".
	HostLocal  = "local"
	HostGlobal = "global"

	EnvPrefix = "COMFY_PROXY_"
	EnvConfig = EnvPrefix + "CONFIG"
)

// Config is the fully resolved configuration. Field tags are the TOML keys.
type Config struct {
	ComfyUI string `toml:"comfyui"`
	Host    string `toml:"host"`
	Port    int    `toml:"port"`
	Token   string `toml:"token"`
	// ComfyUIToken is sent to ComfyUI as a bearer token, for a ComfyUI
	// behind an authenticating gateway. Never forwarded to clients.
	ComfyUIToken      string   `toml:"comfyui_token"`
	ComfyUIBaseDir    string   `toml:"comfyui_base_dir"`
	MaxUploadMB       int      `toml:"max_upload_mb"`
	AllowInsecureBind bool     `toml:"allow_insecure_bind"`
	StateDir          string   `toml:"state_dir"`
	CORSOrigins       []string `toml:"cors_origins"`
	// PublicURL is the origin clients reach the proxy at, used for every URL
	// the proxy puts in a response. Unset, links follow the request's Host,
	// which reads as http behind a TLS-terminating reverse proxy.
	PublicURL string `toml:"public_url"`
	// Quiet turns off the per-request access log.
	Quiet bool `toml:"quiet"`

	// BindIP is Host resolved to the address the listener binds.
	BindIP net.IP `toml:"-"`
	// File is the config file that was read, empty when none was.
	File string `toml:"-"`
	// Sources records where each key's final value came from, for error
	// messages and startup output.
	Sources map[string]string `toml:"-"`
}

// Overrides carries values from one layer above the file. A nil field means
// the layer did not set that key.
type Overrides struct {
	ComfyUI           *string
	Host              *string
	Port              *int
	Token             *string
	ComfyUIToken      *string
	ComfyUIBaseDir    *string
	MaxUploadMB       *int
	AllowInsecureBind *bool
	StateDir          *string
	CORSOrigins       []string
	PublicURL         *string
	Quiet             *bool
}

// Error is a configuration problem the user has to fix. The CLI exits 2 on it.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func errorf(format string, args ...any) error {
	return &Error{msg: fmt.Sprintf(format, args...)}
}

// Load resolves the configuration. configFlag is the --config value (empty
// when unset), lookupEnv is os.LookupEnv outside tests, and flags holds only
// the flags the user actually passed.
func Load(configFlag string, lookupEnv func(string) (string, bool), flags Overrides) (*Config, error) {
	cfg := &Config{
		ComfyUI:     DefaultComfyUI,
		Host:        DefaultHost,
		Port:        DefaultPort,
		MaxUploadMB: DefaultMaxUploadMB,
		Sources:     map[string]string{},
	}
	for _, key := range []string{"comfyui", "host", "port", "max_upload_mb"} {
		cfg.Sources[key] = "default"
	}

	path, explicit, err := configPath(configFlag, lookupEnv)
	if err != nil {
		return nil, err
	}
	if path != "" {
		if err := cfg.readFile(path, explicit); err != nil {
			return nil, err
		}
	}

	env, err := envOverrides(lookupEnv)
	if err != nil {
		return nil, err
	}
	cfg.apply(env, func(key string) string { return "env " + EnvPrefix + strings.ToUpper(key) })
	cfg.apply(flags, func(key string) string { return "flag --" + flagName(key) })

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Path reports which config file Load would read and whether it was named
// explicitly (by --config or COMFY_PROXY_CONFIG). Empty when there is no
// default location to look in.
func Path(configFlag string, lookupEnv func(string) (string, bool)) (string, bool) {
	p, explicit, err := configPath(configFlag, lookupEnv)
	if err != nil {
		return "", false
	}
	return p, explicit
}

// configPath picks the file to read. An explicit path (flag or env) must
// exist; the default location is optional.
func configPath(configFlag string, lookupEnv func(string) (string, bool)) (string, bool, error) {
	if configFlag != "" {
		return configFlag, true, nil
	}
	if p, ok := lookupEnv(EnvConfig); ok && p != "" {
		return p, true, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		// No HOME or XDG_CONFIG_HOME: there is no default file to look for.
		return "", false, nil
	}
	return filepath.Join(dir, "comfy-api-proxy", "config.toml"), false, nil
}

func (c *Config) readFile(path string, explicit bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !explicit {
			return nil
		}
		return errorf("cannot read config file %s: %v", path, err)
	}

	// The generic decode only records which keys the file sets, so they can be
	// told apart from defaults for source tracking.
	var seen map[string]any
	if err := toml.Unmarshal(raw, &seen); err != nil {
		return errorf("config file %s: %v", path, err)
	}

	dec := toml.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		if strict, ok := errors.AsType[*toml.StrictMissingError](err); ok {
			return errorf("config file %s has unknown keys:\n%s", path, strict.String())
		}
		return errorf("config file %s: %v", path, err)
	}
	c.File = path
	for key := range seen {
		c.Sources[key] = "config " + path
	}
	return nil
}

// envOverrides reads the COMFY_PROXY_* layer. An empty variable counts as
// unset, so `COMFY_PROXY_PORT=` in a service file falls through to the
// config file instead of failing as a bad integer.
func envOverrides(lookupEnv func(string) (string, bool)) (Overrides, error) {
	var o Overrides
	get := func(key string) (string, bool) {
		v, ok := lookupEnv(EnvPrefix + key)
		return v, ok && strings.TrimSpace(v) != ""
	}
	str := func(key string) *string {
		if v, ok := get(key); ok {
			return &v
		}
		return nil
	}
	num := func(key string) (*int, error) {
		v, ok := get(key)
		if !ok {
			return nil, nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, errorf("%s%s must be an integer, got %q", EnvPrefix, key, v)
		}
		return &n, nil
	}
	var err error
	o.ComfyUI = str("COMFYUI")
	o.Host = str("HOST")
	o.Token = str("TOKEN")
	o.ComfyUIToken = str("COMFYUI_TOKEN")
	o.ComfyUIBaseDir = str("COMFYUI_BASE_DIR")
	o.StateDir = str("STATE_DIR")
	o.PublicURL = str("PUBLIC_URL")
	if o.Port, err = num("PORT"); err != nil {
		return o, err
	}
	if o.MaxUploadMB, err = num("MAX_UPLOAD_MB"); err != nil {
		return o, err
	}
	boolean := func(key string) (*bool, error) {
		v, ok := get(key)
		if !ok {
			return nil, nil
		}
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return nil, errorf("%s%s must be true or false, got %q", EnvPrefix, key, v)
		}
		return &b, nil
	}
	if o.AllowInsecureBind, err = boolean("ALLOW_INSECURE_BIND"); err != nil {
		return o, err
	}
	if o.Quiet, err = boolean("QUIET"); err != nil {
		return o, err
	}
	if v, ok := get("CORS_ORIGINS"); ok {
		for origin := range strings.SplitSeq(v, ",") {
			if origin = strings.TrimSpace(origin); origin != "" {
				o.CORSOrigins = append(o.CORSOrigins, origin)
			}
		}
	}
	return o, nil
}

func (c *Config) apply(o Overrides, source func(key string) string) {
	setStr := func(key string, dst *string, v *string) {
		if v != nil {
			*dst = *v
			c.Sources[key] = source(key)
		}
	}
	setInt := func(key string, dst *int, v *int) {
		if v != nil {
			*dst = *v
			c.Sources[key] = source(key)
		}
	}
	setStr("comfyui", &c.ComfyUI, o.ComfyUI)
	setStr("host", &c.Host, o.Host)
	setInt("port", &c.Port, o.Port)
	setStr("token", &c.Token, o.Token)
	setStr("comfyui_token", &c.ComfyUIToken, o.ComfyUIToken)
	setStr("comfyui_base_dir", &c.ComfyUIBaseDir, o.ComfyUIBaseDir)
	setInt("max_upload_mb", &c.MaxUploadMB, o.MaxUploadMB)
	setStr("state_dir", &c.StateDir, o.StateDir)
	setStr("public_url", &c.PublicURL, o.PublicURL)
	if o.AllowInsecureBind != nil {
		c.AllowInsecureBind = *o.AllowInsecureBind
		c.Sources["allow_insecure_bind"] = source("allow_insecure_bind")
	}
	if o.CORSOrigins != nil {
		c.CORSOrigins = o.CORSOrigins
		c.Sources["cors_origins"] = source("cors_origins")
	}
	if o.Quiet != nil {
		c.Quiet = *o.Quiet
		c.Sources["quiet"] = source("quiet")
	}
}

// flagName maps a TOML key to its CLI flag.
func flagName(key string) string {
	if key == "cors_origins" {
		return "enable-cors-header"
	}
	return strings.ReplaceAll(key, "_", "-")
}

func (c *Config) validate() error {
	u, err := url.Parse(c.ComfyUI)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errorf("comfyui must be an http(s) URL with a host, got %q (from %s)", c.ComfyUI, c.Sources["comfyui"])
	}
	c.ComfyUI = strings.TrimRight(c.ComfyUI, "/")

	ip, err := ResolveHost(c.Host)
	if err != nil {
		return errorf("%v (from %s)", err, c.Sources["host"])
	}
	c.BindIP = ip

	if c.Port < 1 || c.Port > 65535 {
		return errorf("port must be between 1 and 65535, got %d (from %s)", c.Port, c.Sources["port"])
	}
	if c.MaxUploadMB < 1 {
		return errorf("max_upload_mb must be at least 1, got %d (from %s)", c.MaxUploadMB, c.Sources["max_upload_mb"])
	}

	origins := make([]string, 0, len(c.CORSOrigins))
	for _, raw := range c.CORSOrigins {
		origin, err := NormalizeCORSOrigin(raw)
		if err != nil {
			return errorf("invalid --enable-cors-header: %v (from %s)", err, c.Sources["cors_origins"])
		}
		origins = append(origins, origin)
	}
	c.CORSOrigins = origins

	if c.PublicURL != "" {
		public, err := normalizePublicURL(c.PublicURL)
		if err != nil {
			return errorf("public_url %v, got %q (from %s)", err, c.PublicURL, c.Sources["public_url"])
		}
		c.PublicURL = public
	}

	if !ip.IsLoopback() && c.Token == "" && !c.AllowInsecureBind {
		return errorf("refusing to bind non-loopback host %q (from %s) without a token.\n"+
			"Set a token (--token, %sTOKEN, or token in the config file), "+
			"or set allow_insecure_bind to expose the proxy unauthenticated.",
			c.Host, c.Sources["host"], EnvPrefix)
	}
	return nil
}

// ResolveHost turns a host setting into the IP to bind. It accepts the named
// scopes "local" and "global", "localhost", or a literal IPv4/IPv6 address.
// An empty host is rejected: a socket treats it as every interface, which
// would slip past the loopback check while reading as unset.
func ResolveHost(host string) (net.IP, error) {
	switch strings.TrimSpace(host) {
	case HostLocal, "localhost":
		return net.IPv4(127, 0, 0, 1), nil
	case HostGlobal:
		return net.IPv4zero, nil
	case "":
		return nil, errors.New(`host is empty; use "local", "global", or an IP address`)
	}
	ip := net.ParseIP(strings.Trim(strings.TrimSpace(host), "[]"))
	if ip == nil {
		return nil, fmt.Errorf(`host must be "local", "global", "localhost", or an IP address, got %q`, host)
	}
	return ip, nil
}

// normalizePublicURL accepts an absolute http(s) URL with a host and an
// optional path prefix, and returns it with the scheme and host lowercased
// and no trailing slash, ready to prefix "/api/v2/...".
func normalizePublicURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	switch {
	case err != nil:
		return "", errors.New("must be an absolute http(s) URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return "", errors.New("must start with http:// or https://")
	case u.Host == "" || u.Hostname() == "":
		return "", errors.New("must name a host")
	case u.User != nil:
		return "", errors.New("must not include a user or password")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", errors.New("must not include a query or fragment")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/"), nil
}
