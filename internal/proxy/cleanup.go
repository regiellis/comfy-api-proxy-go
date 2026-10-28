package proxy

import (
	"context"
	"os"
	"strconv"
	"time"
)

// defaultCleanupInterval matches the reference. COMFY_ASSET_CLEANUP_INTERVAL
// (seconds) overrides it, under the same name the reference reads.
const defaultCleanupInterval = 300 * time.Second

func cleanupInterval() time.Duration {
	if raw := os.Getenv("COMFY_ASSET_CLEANUP_INTERVAL"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return defaultCleanupInterval
}

// RunCleanup removes expired assets on an interval until ctx ends. Unlike the
// reference it also runs with --state-dir, where expires_at now persists.
func (p *Proxy) RunCleanup(ctx context.Context) {
	ticker := time.NewTicker(cleanupInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleted, err := p.assets.DeleteExpired(now())
			if err != nil {
				p.logf("error during asset cleanup: %v", err)
			}
			if len(deleted) > 0 {
				p.logf("Cleaned up %d expired asset(s)", len(deleted))
			}
		}
	}
}
