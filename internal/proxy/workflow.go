package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
)

// decodeJSON parses a request body keeping numbers as json.Number, so a
// 64-bit seed in a workflow survives the round trip to ComfyUI intact rather
// than being rounded through float64.
func decodeJSON(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data after JSON value")
	}
	return nil
}

// encodeJSON marshals without HTML escaping, so a stored workflow reads back
// as the client wrote it.
func encodeJSON(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// isUIFormat spots a UI-export graph (top-level nodes list plus links), the
// most common integrator mistake, so it can be rejected with its own code.
func isUIFormat(wf map[string]any) bool {
	_, isList := wf["nodes"].([]any)
	_, hasLinks := wf["links"]
	return isList && hasLinks
}

// isAssetRef reports a node tagged as a core/ASSET reference. Whether its
// info block is well formed is checked later, so a malformed reference is
// rejected rather than forwarded to ComfyUI as a literal object.
func isAssetRef(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	tag, _ := m["__type"].(string)
	return m, tag == "core/ASSET"
}

// rewriteAssetRefs replaces each core/ASSET reference with the filename
// ComfyUI expects, collecting the ones that do not resolve.
func (p *Proxy) rewriteAssetRefs(node any, missing *[]string) any {
	if m, ok := isAssetRef(node); ok {
		info, ok := m["info"].(map[string]any)
		if !ok {
			*missing = append(*missing, "<malformed core/ASSET: missing info>")
			return node
		}
		resolved, ok := p.resolveAssetRef(info)
		if !ok {
			*missing = append(*missing, pythonStr(info, "id"))
			return node
		}
		return resolved
	}
	switch v := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			out[k] = p.rewriteAssetRefs(child, missing)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = p.rewriteAssetRefs(child, missing)
		}
		return out
	}
	return node
}

// resolveAssetRef resolves one info block to the filename a loader widget
// would carry: an uploaded asset by id, a stateless output id used as an
// input, or an asset found by hash.
func (p *Proxy) resolveAssetRef(info map[string]any) (string, bool) {
	var rec AssetRecord
	found := false
	if id, ok := info["id"].(string); ok {
		rec, found = p.assets.Get(id)
		if !found {
			if ref, ok := p.decodeOutputAssetID(id); ok {
				return joinSubfolder(ref.Subfolder, ref.Filename), true
			}
		}
	}
	if !found {
		if hash, ok := info["hash"].(string); ok {
			rec, found = p.assets.GetByHash(hash)
		}
	}
	if !found {
		return "", false
	}
	if rec.ComfyRef != nil {
		return joinSubfolder(rec.ComfyRef.Subfolder, rec.ComfyRef.Filename), true
	}
	// A model file placed on disk is referenced by its root-relative path.
	if rec.FilePath != nil {
		return *rec.FilePath, true
	}
	return "", false
}

func joinSubfolder(subfolder, filename string) string {
	if subfolder == "" {
		return filename
	}
	return path.Join(subfolder, filename)
}

// pythonStr renders m[key] the way Python's str() would, for the missing_ids
// list the reference returns.
func pythonStr(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return "<no id>"
	}
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		return t.String()
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}
