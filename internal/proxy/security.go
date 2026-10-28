package proxy

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Two trust boundaries meet here: where an uploaded file may land (an
// allowlist of real ComfyUI roots, no traversal, no symlink escape, no
// clobbering), and what may land in a model directory (a header-checked
// safetensors file, never a pickle a loader would execute).

// inputRoots are where a plain workflow-input upload may land.
var inputRoots = map[string]bool{"input": true}

// modelRoots are the keys of ComfyUI's folder_paths.folder_names_and_paths
// that hold weights. custom_nodes (arbitrary Python) and configs (arbitrary
// YAML read by loaders) are registered there too and deliberately left out.
var modelRoots = map[string]bool{
	"checkpoints": true, "loras": true, "vae": true, "text_encoders": true,
	"diffusion_models": true, "clip_vision": true, "style_models": true,
	"embeddings": true, "diffusers": true, "vae_approx": true, "controlnet": true,
	"gligen": true, "upscale_models": true, "latent_upscale_models": true,
	"hypernetworks": true, "photomaker": true, "classifiers": true,
	"model_patches": true, "audio_encoders": true, "background_removal": true,
	"frame_interpolation": true, "geometry_estimation": true, "optical_flow": true,
}

// errPlacement is a requested file_path that is not an acceptable target.
type errPlacement struct{ msg string }

func (e *errPlacement) Error() string { return e.msg }

func placementf(format string, args ...any) error {
	return &errPlacement{msg: fmt.Sprintf(format, args...)}
}

// pyRepr quotes s the way Python's repr() does for plain strings, so error
// messages match the reference.
func pyRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

func allowedRoots() string {
	names := make([]string, 0, len(inputRoots)+len(modelRoots))
	for k := range inputRoots {
		names = append(names, k)
	}
	for k := range modelRoots {
		names = append(names, k)
	}
	sort.Strings(names)
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + n + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// pathParts splits like Python's PurePosixPath.parts on a relative path:
// empty and "." segments vanish, so "a//./b/" is ["a", "b"].
func pathParts(p string) []string {
	var parts []string
	for seg := range strings.SplitSeq(p, "/") {
		if seg != "" && seg != "." {
			parts = append(parts, seg)
		}
	}
	return parts
}

// validateUploadPath classifies a client file_path before anything else
// touches it. Checking the whole path for ".." first closes the bypass where
// "input/../checkpoints/x.safetensors" classifies as an input by its first
// segment and skips the model guards. It returns whether the path is a model
// placement and the validated remainder: category-qualified for a model
// ("checkpoints/x.safetensors"), input-relative for an input ("x.png").
func validateUploadPath(filePath string) (bool, string, error) {
	if filePath == "" || strings.HasPrefix(filePath, "/") || strings.HasPrefix(filePath, `\`) {
		return false, "", placementf("file_path must be relative: %s", pyRepr(filePath))
	}
	stripped := strings.TrimPrefix(filePath, "models/")
	parts := pathParts(stripped)
	if len(parts) == 0 {
		return false, "", placementf("file_path must include a filename: %s", pyRepr(filePath))
	}
	for _, p := range parts {
		if p == ".." {
			return false, "", placementf("file_path must not contain '..': %s", pyRepr(filePath))
		}
	}
	if len(parts) < 2 {
		// A bare filename is the implicit-input convention.
		return false, strings.Join(parts, "/"), nil
	}
	root := parts[0]
	if !modelRoots[root] && !inputRoots[root] {
		return false, "", placementf("'%s' is not an allowlisted placement root (allowed: %s)", root, allowedRoots())
	}
	if modelRoots[root] {
		return true, strings.Join(parts, "/"), nil
	}
	return false, strings.Join(parts[1:], "/"), nil
}

// resolvePlacementPath turns a category-qualified model path into an
// absolute destination under baseDir, refusing traversal, non-safetensors
// names, a resolved path outside baseDir (a symlink anywhere along the way),
// and an existing file.
func resolvePlacementPath(baseDir, filePath string) (string, error) {
	if filePath == "" || strings.HasPrefix(filePath, "/") || strings.HasPrefix(filePath, `\`) {
		return "", placementf("file_path must be relative: %s", pyRepr(filePath))
	}
	parts := pathParts(filePath)
	for _, p := range parts {
		if p == ".." {
			return "", placementf("file_path must not contain '..': %s", pyRepr(filePath))
		}
	}
	if len(parts) < 2 {
		return "", placementf("file_path must include a root directory (e.g. 'input/x.png' or "+
			"'checkpoints/x.safetensors'): %s", pyRepr(filePath))
	}
	root := parts[0]
	isModel := modelRoots[root]
	if !isModel && !inputRoots[root] {
		return "", placementf("'%s' is not an allowlisted placement root (allowed: %s)", root, allowedRoots())
	}
	if isModel && strings.ToLower(filepath.Ext(parts[len(parts)-1])) != ".safetensors" {
		return "", placementf("model uploads must be .safetensors, got '%s'", filePath)
	}

	baseReal, err := resolveLenient(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve base dir: %w", err)
	}
	candidate, err := resolveLenient(filepath.Join(append([]string{baseReal}, parts...)...))
	if err != nil {
		return "", fmt.Errorf("resolve destination: %w", err)
	}
	rel, err := filepath.Rel(baseReal, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", placementf("resolved path '%s' escapes the allowed root '%s' (possible symlink escape)", candidate, baseReal)
	}
	if _, err := os.Stat(candidate); err == nil {
		return "", placementf("refusing to overwrite existing file: %s", candidate)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("stat destination: %w", err)
	}
	return candidate, nil
}

// resolveLenient is Python's non-strict Path.resolve: symlinks are followed
// on the longest prefix that exists, and the rest is appended as written.
func resolveLenient(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	existing, rest := abs, ""
	for {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			if rest == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
}

// atomicNoClobberWrite copies src into destination without ever exposing a
// torn file or overwriting one. Bytes go to a sibling temp file first, so the
// final rename stays on one filesystem. An O_EXCL create on the destination
// is the last no-clobber gate, closing the gap after resolvePlacementPath's
// existence check.
func atomicNoClobberWrite(destination string, src io.Reader) (err error) {
	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(destination)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) && err == nil {
			err = fmt.Errorf("remove temp file: %w", rmErr)
		}
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		return errors.Join(fmt.Errorf("write temp file: %w", err), tmp.Close())
	}
	// CreateTemp makes the file 0600. ComfyUI may run as another user, so
	// the placed model gets the usual 0644.
	if err := tmp.Chmod(0o644); err != nil {
		return errors.Join(fmt.Errorf("set model file mode: %w", err), tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	gate, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return placementf("refusing to overwrite existing file: %s", destination)
		}
		return fmt.Errorf("claim destination: %w", err)
	}
	if err := gate.Close(); err != nil {
		return fmt.Errorf("close destination: %w", err)
	}
	if err := os.Rename(tmpName, destination); err != nil {
		return fmt.Errorf("move into place: %w", err)
	}
	return nil
}

// maxSafetensorsHeader is safetensors' own bound on header size.
const maxSafetensorsHeader = 100 * 1024 * 1024

// looksLikeSafetensors checks the header of the file at r: an 8-byte little-
// endian length, then that many bytes of JSON mapping tensor names to
// {dtype, shape, data_offsets}, plus an optional __metadata__. The tensor
// bytes are never read. size is the total file size.
func looksLikeSafetensors(r io.ReaderAt, size int64) bool {
	if size < 8 {
		return false
	}
	var lenBuf [8]byte
	if _, err := r.ReadAt(lenBuf[:], 0); err != nil {
		return false
	}
	n := binary.LittleEndian.Uint64(lenBuf[:])
	if n == 0 || n > maxSafetensorsHeader || n > uint64(size-8) {
		return false
	}
	header := make([]byte, n)
	if _, err := r.ReadAt(header, 8); err != nil {
		return false
	}
	// Go's decoder replaces invalid UTF-8 where Python's rejects it.
	if !utf8.Valid(header) {
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(header, &obj) != nil || obj == nil {
		return false
	}
	for key, value := range obj {
		if key == "__metadata__" {
			continue
		}
		var tensor map[string]json.RawMessage
		if json.Unmarshal(value, &tensor) != nil || tensor == nil {
			return false
		}
		for _, field := range []string{"dtype", "shape", "data_offsets"} {
			if _, ok := tensor[field]; !ok {
				return false
			}
		}
	}
	return true
}
