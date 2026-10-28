package config

import (
	"strings"
	"testing"
)

func TestNormalizeCORSOrigin(t *testing.T) {
	ok := map[string]string{
		"https://app.example.com":       "https://app.example.com",
		"https://app.example.com/":      "https://app.example.com",
		"  http://localhost:3000  ":     "http://localhost:3000",
		"http://app.example.com:80":     "http://app.example.com",
		"https://app.example.com:443":   "https://app.example.com",
		"https://app.example.com:8443":  "https://app.example.com:8443",
		"http://[::1]:5173":             "http://[::1]:5173",
		"http://app.example.com:80/":    "http://app.example.com",
		"https://APP.example.com:44300": "https://app.example.com:44300",
		"HTTPS://App.Example.com":       "https://app.example.com",
	}
	for in, want := range ok {
		got, err := NormalizeCORSOrigin(in)
		if err != nil || got != want {
			t.Errorf("NormalizeCORSOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	bad := map[string]string{
		"":                              "non-empty",
		"*":                             "'*' is not allowed",
		"app.example.com":               "absolute http(s) URL",
		"ftp://app.example.com":         "absolute http(s) URL",
		"https://user:pw@app.example":   "userinfo",
		"https://app.example.com:":      "empty port",
		"https://app.example.com:99999": "invalid port",
		"https://app.example.com:abc":   "",
		"https://app.example.com/path":  "path, query, or fragment",
		"https://app.example.com/?q=1":  "path, query, or fragment",
		"https://app.example.com/#f":    "path, query, or fragment",
	}
	for in, want := range bad {
		_, err := NormalizeCORSOrigin(in)
		if err == nil {
			t.Errorf("NormalizeCORSOrigin(%q) succeeded, want error", in)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("NormalizeCORSOrigin(%q) error %q, want it to mention %q", in, err, want)
		}
	}
}
