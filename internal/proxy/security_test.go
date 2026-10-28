package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePlacementPath(t *testing.T) {
	base := t.TempDir()
	for _, ok := range []string{"input/x.png", "checkpoints/model.safetensors", "loras/sub/dir/x.SAFETENSORS"} {
		if _, err := resolvePlacementPath(base, ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	rejects := map[string]string{
		"/etc/passwd":                     "must be relative",
		`\windows\x`:                      "must be relative",
		"input/../../etc/passwd":          "'..'",
		"custom_nodes/evil.py":            "not an allowlisted placement root",
		"configs/x.yaml":                  "not an allowlisted placement root",
		"checkpoints":                     "must include a root directory",
		"checkpoints/model.ckpt":          "must be .safetensors",
		"checkpoints/model.pt":            "must be .safetensors",
		"checkpoints/model.safetensors.p": "must be .safetensors",
	}
	for p, want := range rejects {
		_, err := resolvePlacementPath(base, p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want %q", p, err, want)
		}
	}
}

func TestResolvePlacementPathSymlinkEscape(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "checkpoints")); err != nil {
		t.Fatal(err)
	}
	_, err := resolvePlacementPath(base, "checkpoints/x.safetensors")
	if err == nil || !strings.Contains(err.Error(), "escapes the allowed root") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolvePlacementPathRefusesExisting(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "loras")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.safetensors"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := resolvePlacementPath(base, "loras/x.safetensors")
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateUploadPath(t *testing.T) {
	cases := []struct {
		in      string
		isModel bool
		norm    string
	}{
		{"input/photo.png", false, "photo.png"},
		{"input/sub/photo.png", false, "sub/photo.png"},
		{"photo.png", false, "photo.png"},
		{"checkpoints/m.safetensors", true, "checkpoints/m.safetensors"},
		{"models/checkpoints/m.safetensors", true, "checkpoints/m.safetensors"},
		{"input//./photo.png", false, "photo.png"},
	}
	for _, tc := range cases {
		isModel, norm, err := validateUploadPath(tc.in)
		if err != nil || isModel != tc.isModel || norm != tc.norm {
			t.Errorf("%s = (%v, %q, %v), want (%v, %q)", tc.in, isModel, norm, err, tc.isModel, tc.norm)
		}
	}
	rejects := map[string]string{
		"input/../checkpoints/evil.safetensors": "'..'",
		"input/../../../../etc/passwd":          "'..'",
		"models/../checkpoints/x.safetensors":   "'..'",
		"/abs/x.png":                            "must be relative",
		"":                                      "must be relative",
		"custom_nodes/x.py":                     "not an allowlisted placement root",
		"./":                                    "must include a filename",
	}
	for in, want := range rejects {
		_, _, err := validateUploadPath(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", in, err, want)
		}
	}
}

func TestAtomicNoClobberWrite(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "loras", "x.safetensors")
	if err := atomicNoClobberWrite(dest, strings.NewReader("weights")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "weights" {
		t.Fatalf("content = %q, %v", got, err)
	}

	err = atomicNoClobberWrite(dest, strings.NewReader("other"))
	var pe *errPlacement
	if !errors.As(err, &pe) {
		t.Fatalf("second write err = %v, want placement error", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "weights" {
		t.Fatalf("existing file clobbered: %q", got)
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func safetensors(header string) []byte {
	var buf bytes.Buffer
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(header)))
	buf.Write(n[:])
	buf.WriteString(header)
	buf.WriteString("tensor-bytes")
	return buf.Bytes()
}

func TestLooksLikeSafetensors(t *testing.T) {
	check := func(b []byte) bool { return looksLikeSafetensors(bytes.NewReader(b), int64(len(b))) }
	if !check(safetensors(`{"w":{"dtype":"F32","shape":[1],"data_offsets":[0,4]}}`)) {
		t.Error("well-formed header rejected")
	}
	if !check(safetensors(`{"__metadata__":{"format":"pt"}}`)) {
		t.Error("metadata-only header rejected")
	}
	tooLong := safetensors(`{}`)
	binary.LittleEndian.PutUint64(tooLong[:8], 1<<20)
	for name, b := range map[string][]byte{
		"too short":       []byte("abc"),
		"garbage header":  safetensors("not json"),
		"len past buffer": tooLong,
		"pickle magic":    append([]byte{0x80, 0x02}, bytes.Repeat([]byte{0}, 20)...),
		"missing keys":    safetensors(`{"w":{"dtype":"F32"}}`),
		"not an object":   safetensors(`[1,2]`),
		"invalid utf-8":   safetensors("{\"\xff\":{}}"),
	} {
		if check(b) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAtomicNoClobberWriteModeIsWorldReadable(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "x.safetensors")
	if err := atomicNoClobberWrite(dest, strings.NewReader("w")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode = %o, want 644 so a ComfyUI under another user can load it", got)
	}
}
