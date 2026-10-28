package proxy

import (
	"context"
	"net/url"
	"sync"
	"time"
)

const (
	// maxSizeCache bounds the remembered output sizes; past it the cache
	// starts over, which costs one HEAD per output on the next read.
	maxSizeCache = 4096
	// sizeLookups bounds the HEAD requests one job read sends at once.
	sizeLookups = 8
	sizeTimeout = 5 * time.Second
)

// sizeCache remembers output sizes by signed output id. An output id names
// one file in ComfyUI's output folder, and ComfyUI never rewrites one in
// place, so a size read once stays true. Failures are not remembered.
type sizeCache struct {
	mu sync.Mutex
	m  map[string]int64
}

func (c *sizeCache) get(id string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.m[id]
	return n, ok
}

func (c *sizeCache) put(id string, n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= maxSizeCache {
		c.m = make(map[string]int64)
	}
	c.m[id] = n
}

// fillSizes sets SizeBytes on each output from ComfyUI's /view. The
// reference reports 0 for every output (Comfy-Org/comfy-api-proxy#26). An
// output ComfyUI cannot size stays 0 and the read still succeeds.
func (p *Proxy) fillSizes(ctx context.Context, outs []output) {
	sem := make(chan struct{}, sizeLookups)
	var wg sync.WaitGroup
	for i := range outs {
		if n, ok := p.sizes.get(outs[i].ID); ok {
			outs[i].SizeBytes = n
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			outs[i].SizeBytes = p.outputSize(ctx, outs[i].ID, outs[i].ref)
		})
	}
	wg.Wait()
}

// outputSize returns the size of one output file, from the cache or a HEAD
// of /view, and 0 when ComfyUI cannot say.
func (p *Proxy) outputSize(ctx context.Context, id string, ref ComfyRef) int64 {
	if n, ok := p.sizes.get(id); ok {
		return n
	}
	typ := ref.Type
	if typ == "" {
		typ = "output"
	}
	q := url.Values{"filename": {ref.Filename}, "subfolder": {ref.Subfolder}, "type": {typ}}
	ctx, cancel := context.WithTimeout(ctx, sizeTimeout)
	defer cancel()
	n, ok, err := p.comfy.Size(ctx, "/view?"+q.Encode())
	if err != nil {
		p.logf("warning: size output %s: %v", ref.Filename, err)
		return 0
	}
	if !ok {
		return 0
	}
	p.sizes.put(id, n)
	return n
}
