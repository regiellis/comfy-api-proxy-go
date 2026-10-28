// Package server owns the listener and the HTTP server lifecycle.
package server

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
)

// PortInUseError reports that the configured port is already bound. The
// proxy never falls back to another port: SDK clients are pointed at a fixed
// one, and a port that moves silently breaks them.
type PortInUseError struct {
	Addr string
	Err  error
}

func (e *PortInUseError) Error() string {
	return fmt.Sprintf("port already in use: %s", e.Addr)
}

func (e *PortInUseError) Unwrap() error { return e.Err }

// Listen binds ip:port. It uses tcp4 or tcp6 to match the address family of
// ip, so binding 0.0.0.0 never quietly becomes a dual-stack socket that also
// listens on IPv6 addresses the startup output did not print.
func Listen(ip net.IP, port int) (net.Listener, error) {
	network := "tcp6"
	if ip.To4() != nil {
		network = "tcp4"
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	ln, err := net.Listen(network, addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, &PortInUseError{Addr: addr, Err: err}
		}
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}

// bridgePrefixes name the host-side interfaces of local containers and VMs.
// Their addresses are reachable only from this machine's own guests, and on
// a Docker host there can be dozens, burying the LAN and VPN URLs a remote
// client actually needs.
var bridgePrefixes = []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "vmnet", "vboxnet", "lxcbr", "lxdbr", "podman"}

func isBridge(name string) bool {
	for _, p := range bridgePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// URLs lists every base URL the proxy is reachable at when bound to ip:port.
// A wildcard bind expands to loopback plus the address of each interface
// that is up, in the bound family, skipping container and VM bridges. IPv6
// link-local addresses are skipped because they are unusable in a URL
// without a zone.
func URLs(ip net.IP, port int) ([]string, error) {
	p := strconv.Itoa(port)
	format := func(ip net.IP) string { return "http://" + net.JoinHostPort(ip.String(), p) }
	if !ip.IsUnspecified() {
		return []string{format(ip)}, nil
	}

	v4 := ip.To4() != nil
	urls := []string{format(net.IPv4(127, 0, 0, 1))}
	if !v4 {
		urls = []string{format(net.IPv6loopback)}
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list network interfaces: %w", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || isBridge(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, fmt.Errorf("list addresses of %s: %w", iface.Name, err)
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || (ipnet.IP.To4() != nil) != v4 || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			urls = append(urls, format(ipnet.IP))
		}
	}
	return urls, nil
}
