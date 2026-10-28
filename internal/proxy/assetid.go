package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf16"
)

// outputRef is the signed payload of a stateless output asset id.
type outputRef struct {
	Job       *string `json:"j"`
	Filename  *string `json:"f"`
	Subfolder *string `json:"s"`
	Type      *string `json:"t"`
}

// outputAssetID encodes a ComfyUI output reference into a stateless,
// deterministic id: base64url(JSON payload) "." base64url(HMAC-SHA256 tag).
// The job id is inside the signed payload so two jobs that write the same
// filename (a fixed prefix, or a counter reset by a restart) get distinct
// ids, and a client holding job A's URL is never served job B's bytes. The
// signature stops a client from forging an id that names an arbitrary file
// /view can reach.
func (p *Proxy) outputAssetID(jobID, filename, subfolder, typ string) string {
	// Written byte for byte as Python's json.dumps writes it, so the same
	// output gets the same id from either implementation sharing a secret.
	raw := `{"j": ` + pyJSONString(jobID) + `, "f": ` + pyJSONString(filename) +
		`, "s": ` + pyJSONString(subfolder) + `, "t": ` + pyJSONString(typ) + `}`
	payload := base64.RawURLEncoding.EncodeToString([]byte(raw))
	return payload + "." + base64.RawURLEncoding.EncodeToString(p.sign(payload))
}

// pyJSONString quotes s as Python's json.dumps does with its default
// ensure_ascii: printable ASCII stays, the usual short escapes apply, and
// everything else becomes lowercase \uXXXX, with surrogate pairs above U+FFFF.
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= 0x20 && r <= 0x7e:
				b.WriteRune(r)
			case r > 0xffff:
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			default:
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (p *Proxy) sign(payload string) []byte {
	mac := hmac.New(sha256.New, p.secret)
	if _, err := mac.Write([]byte(payload)); err != nil {
		return nil // hash writes do not fail; a nil tag never verifies
	}
	return mac.Sum(nil)
}

// decodeOutputAssetID verifies and decodes an id minted by outputAssetID. Any
// malformed or wrongly signed id, including a well-formed forgery, yields
// ok=false, which every caller treats as "not found". Uploaded asset ids are
// UUIDs with no dot, so they never decode.
func (p *Proxy) decodeOutputAssetID(id string) (ComfyRef, bool) {
	ref, _, ok := p.decodeOutputAssetIDJob(id)
	return ref, ok
}

// decodeOutputAssetIDJob also returns the job id from the payload, nil for an
// id minted before outputs were scoped per job.
func (p *Proxy) decodeOutputAssetIDJob(id string) (ComfyRef, *string, bool) {
	payload, tagB64, found := strings.Cut(id, ".")
	if !found {
		return ComfyRef{}, nil, false
	}
	tag, err := decodeB64URL(tagB64)
	if err != nil || !hmac.Equal(tag, p.sign(payload)) {
		return ComfyRef{}, nil, false
	}
	raw, err := decodeB64URL(payload)
	if err != nil {
		return ComfyRef{}, nil, false
	}
	var ref outputRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return ComfyRef{}, nil, false
	}
	if ref.Filename == nil || ref.Subfolder == nil || ref.Type == nil {
		return ComfyRef{}, nil, false
	}
	return ComfyRef{Filename: *ref.Filename, Subfolder: *ref.Subfolder, Type: *ref.Type}, ref.Job, true
}

// decodeB64URL accepts base64url with or without padding.
func decodeB64URL(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
