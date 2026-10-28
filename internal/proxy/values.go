package proxy

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/internal/httpx"
)

// Value outputs are strings a node returns in place of a file, such as
// PreviewAny's text: ComfyUI's history holds them as raw strings beside or
// instead of file entries. The reference drops them (Comfy-Org/
// comfy-api-proxy#22), so a job that only computes a value reports no
// outputs. Here each becomes an ordinary output entry whose content is
// served as text/plain from the history, with no copy kept by the proxy.

// valueRef names one value: the job, the output node, the history key, and
// the position in that key's list.
type valueRef struct {
	job, node, key string
	index          int
}

// valuePayload is the signed payload of a value id. It has no "f", so it can
// never decode as a file id, and a file id lacks "n", "k", and "i".
type valuePayload struct {
	Job      *string `json:"j"`
	Node     *string `json:"n"`
	Key      *string `json:"k"`
	Index    *int    `json:"i"`
	Filename *string `json:"f"`
}

// valueAssetID mints a stateless, signed id for one value, the way
// outputAssetID does for a file.
func (p *Proxy) valueAssetID(job, node, key string, index int) string {
	raw, err := json.Marshal(struct {
		Job   string `json:"j"`
		Node  string `json:"n"`
		Key   string `json:"k"`
		Index int    `json:"i"`
	}{job, node, key, index})
	if err != nil {
		return "" // strings and an int always encode; an empty id never verifies
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(p.sign(payload))
}

// decodeValueAssetID verifies a value id. Anything else, a file id
// included, is ok=false.
func (p *Proxy) decodeValueAssetID(id string) (valueRef, bool) {
	payload, tagB64, found := strings.Cut(id, ".")
	if !found {
		return valueRef{}, false
	}
	tag, err := decodeB64URL(tagB64)
	if err != nil || !hmac.Equal(tag, p.sign(payload)) {
		return valueRef{}, false
	}
	raw, err := decodeB64URL(payload)
	if err != nil {
		return valueRef{}, false
	}
	var v valuePayload
	if err := json.Unmarshal(raw, &v); err != nil {
		return valueRef{}, false
	}
	if v.Job == nil || v.Node == nil || v.Key == nil || v.Index == nil || v.Filename != nil || *v.Index < 0 {
		return valueRef{}, false
	}
	return valueRef{job: *v.Job, node: *v.Node, key: *v.Key, index: *v.Index}, true
}

// historyValue reads one value back from ComfyUI's history. found is false
// when the job, node, key, or index is gone or the item is not a string.
func (p *Proxy) historyValue(ctx context.Context, ref valueRef) (value string, found bool, err error) {
	if !validJobID(ref.job) {
		return "", false, nil
	}
	raw, ok, err := p.comfy.GetJSON(ctx, "/history/"+ref.job)
	if err != nil || !ok {
		return "", false, err
	}
	var hist map[string]struct {
		Outputs map[string]map[string][]json.RawMessage `json:"outputs"`
	}
	if json.Unmarshal(raw, &hist) != nil {
		return "", false, nil
	}
	items := hist[ref.job].Outputs[ref.node][ref.key]
	if ref.index >= len(items) {
		return "", false, nil
	}
	if json.Unmarshal(items[ref.index], &value) != nil {
		return "", false, nil
	}
	return value, true, nil
}

// serveValue answers GET or HEAD /assets/{id}/content for a value id. The
// type is pinned to text/plain with nosniff, so a value can never be
// rendered as HTML or run as script.
func (p *Proxy) serveValue(w http.ResponseWriter, r *http.Request, ref valueRef) {
	value, found, err := p.historyValue(r.Context(), ref)
	if err != nil {
		upstreamError(w, "Failed to read the value from ComfyUI's history.")
		return
	}
	if !found {
		httpx.WriteError(w, http.StatusNotFound, "output_unavailable",
			"The value is no longer in ComfyUI's history. "+outputUnavailableMsg, nil)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", time.Time{}, strings.NewReader(value))
}
