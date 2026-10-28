package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// NormalizeCORSOrigin validates one allowlisted browser origin and returns it
// in the form browsers send in the Origin header: http(s)://host[:port], with
// default ports dropped. A "*" wildcard is refused, since authenticated
// browser traffic must never get an unrestricted Access-Control-Allow-Origin.
func NormalizeCORSOrigin(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", errors.New("CORS origin must be a non-empty http(s) URL")
	}
	if raw == "*" {
		return "", errors.New("CORS origin '*' is not allowed; pass explicit origins such as https://app.example.com")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("CORS origin must be an absolute http(s) URL with a host, got %q", value)
	}
	if u.User != nil {
		return "", fmt.Errorf("CORS origin must not include userinfo, got %q", value)
	}
	// Browsers send the host in lower case, and the reference lowercases it
	// too (urlparse's hostname), so a mixed-case entry still matches.
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return "", fmt.Errorf("CORS origin must be an absolute http(s) URL with a host, got %q", value)
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("CORS origin has an empty port, got %q", value)
	}
	port := 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return "", fmt.Errorf("CORS origin has an invalid port, got %q", value)
		}
		port = n
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("CORS origin must not include a path, query, or fragment, got %q", value)
	}

	host := hostname
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == 0 || (u.Scheme == "http" && port == 80) || (u.Scheme == "https" && port == 443) {
		return u.Scheme + "://" + host, nil
	}
	return u.Scheme + "://" + host + ":" + strconv.Itoa(port), nil
}
