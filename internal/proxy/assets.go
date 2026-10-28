package proxy

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lukechampine.com/blake3"

	"github.com/regiellis/comfy-api-proxy-go/internal/httpx"
)

const outputUnavailableMsg = "Retrying cannot help; re-execute with a changed prompt hash."

type assetBody struct {
	ID           string  `json:"id"`
	Hash         *string `json:"hash"`
	SizeBytes    int64   `json:"size_bytes"`
	ContentType  string  `json:"content_type"`
	FilePath     *string `json:"file_path"`
	CreatedAt    string  `json:"created_at"`
	URL          string  `json:"url"`
	URLExpiresAt string  `json:"url_expires_at"`
	CreatedNew   *bool   `json:"created_new,omitempty"`
	ExpiresAt    *string `json:"expires_at,omitempty"`
	JobID        *string `json:"job_id,omitempty"`
}

func assetJSON(rec AssetRecord, createdNew *bool, base string) assetBody {
	var hash *string
	if rec.Hash != "" {
		h := rec.Hash
		hash = &h
	}
	return assetBody{
		ID:           rec.ID,
		Hash:         hash,
		SizeBytes:    rec.SizeBytes,
		ContentType:  rec.ContentType,
		FilePath:     rec.FilePath,
		CreatedAt:    rec.CreatedAt,
		URL:          base + "/api/v2/assets/" + rec.ID + "/content",
		URLExpiresAt: iso(now().Add(retention)),
		CreatedNew:   createdNew,
		ExpiresAt:    rec.ExpiresAt,
	}
}

func boolPtr(b bool) *bool { return &b }

func strPtr(s string) *string { return &s }

// maxExpiry is the last instant an expires_at can name, the end of the
// four-digit years Python's datetime and ISO 8601 timestamps share.
var maxExpiry = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// expiresAt turns expires_in seconds into a timestamp. Zero means no expiry.
// The sum is done in Unix seconds, since time.Duration overflows past about
// 292 years and would wrap a "never expire" value into the past.
func expiresAt(seconds int64) *string {
	if seconds == 0 {
		return nil
	}
	return strPtr(iso(time.Unix(now().Unix()+seconds, 0)))
}

// validExpiresIn reports whether seconds lands inside the representable range.
func validExpiresIn(seconds int64) bool {
	t := now().Unix()
	return seconds <= maxExpiry.Unix()-t && seconds >= -t
}

// hasControl reports CR, LF, or any other control character, which must not
// reach the multipart headers of the upstream upload.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// maxUploadParts bounds the parts one upload may carry, so a body of tiny
// fields cannot pile up in memory.
const maxUploadParts = 64

// upload is a received file part, spooled to a temp file while hashing.
type upload struct {
	file        *os.File
	size        int64
	hash        string
	contentType string
}

func (u *upload) cleanup() error {
	if u == nil || u.file == nil {
		return nil
	}
	name := u.file.Name()
	closeErr := u.file.Close()
	if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(closeErr, err)
	}
	return closeErr
}

var errTooLarge = errors.New("upload too large")

// spool streams a file part to a temp file, hashing as it goes and stopping
// at max bytes so a never-ending upload cannot exhaust the disk.
func spool(part io.Reader, contentType string, max int64) (*upload, error) {
	f, err := os.CreateTemp("", "comfy-upload-")
	if err != nil {
		return nil, fmt.Errorf("create upload temp file: %w", err)
	}
	u := &upload{file: f, contentType: contentType}
	h := blake3.New(32, nil)
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(part, max+1))
	u.size = n
	if err != nil {
		return u, fmt.Errorf("read upload: %w", err)
	}
	if n > max {
		return u, errTooLarge
	}
	u.hash = "blake3:" + hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return u, fmt.Errorf("rewind upload: %w", err)
	}
	return u, nil
}

func (p *Proxy) uploadAsset(w http.ResponseWriter, r *http.Request) {
	base := p.base(r)
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Expected multipart/form-data.", nil)
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Malformed multipart body.", nil)
		return
	}

	fields := map[string]string{}
	tags := []string{}
	var expiresIn int64
	parts := 0
	var up *upload
	defer func() {
		if err := up.cleanup(); err != nil {
			p.logf("error: remove upload temp file: %v", err)
		}
	}()

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Malformed multipart body.", nil)
			return
		}
		parts++
		if parts > maxUploadParts {
			httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request",
				fmt.Sprintf("Upload carries more than %d parts.", maxUploadParts), nil)
			return
		}
		name := part.FormName()
		switch name {
		case "":
			continue
		case "file":
			ct := part.Header.Get("Content-Type")
			if ct == "" {
				ct = "application/octet-stream"
			}
			if err := up.cleanup(); err != nil {
				p.logf("error: remove upload temp file: %v", err)
			}
			up, err = spool(part, ct, p.maxUpload)
			if errors.Is(err, errTooLarge) {
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
					fmt.Sprintf("Upload exceeds %d bytes.", p.maxUpload), nil)
				return
			}
			if err != nil {
				httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Malformed multipart body.", nil)
				return
			}
		default:
			raw, err := io.ReadAll(io.LimitReader(part, 1<<20))
			if err != nil {
				httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Malformed multipart body.", nil)
				return
			}
			value := strings.TrimSpace(string(raw))
			switch name {
			case "tags":
				tags = append(tags, value)
			case "expires_in":
				n, err := strconv.ParseInt(value, 10, 64)
				if errors.Is(err, strconv.ErrRange) || (err == nil && !validExpiresIn(n)) {
					httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "'expires_in' is out of range.", nil)
					return
				}
				if err == nil {
					expiresIn = n
				}
			default:
				fields[name] = value
			}
		}
	}

	if up == nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Missing 'file' part.", nil)
		return
	}
	filePath := fields["file_path"]
	if filePath == "" {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Missing 'file_path'.", nil)
		return
	}
	if hasControl(filePath) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "'file_path' must not contain control characters.", nil)
		return
	}
	isModel, norm, err := validateUploadPath(filePath)
	if err != nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", err.Error(), nil)
		return
	}
	contentType := fields["content_type"]
	if contentType == "" {
		contentType = up.contentType
	}
	if hasControl(contentType) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "'content_type' must not contain control characters.", nil)
		return
	}
	if expected := fields["expected_hash"]; expected != "" && !strings.EqualFold(expected, up.hash) {
		httpx.WriteError(w, http.StatusConflict, "hash_mismatch",
			"Client-declared hash does not match the received bytes.", nil)
		return
	}
	// Dedup: bytes already held return the existing asset.
	if existing, ok := p.assets.GetByHash(up.hash); ok {
		httpx.WriteJSON(w, http.StatusOK, assetJSON(existing, boolPtr(false), base))
		return
	}

	var rec AssetRecord
	if isModel {
		rec, err = p.placeModelFile(w, norm, up, contentType, tags, expiresIn)
	} else {
		rec, err = p.uploadInput(w, r, norm, filePath, up, contentType, tags, expiresIn)
	}
	if err != nil {
		if !errors.Is(err, errResponded) {
			p.internalError(w, err)
		}
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, assetJSON(rec, boolPtr(true), base))
}

// errResponded means the helper already wrote the client's error response.
var errResponded = errors.New("response written")

func (p *Proxy) placeModelFile(w http.ResponseWriter, norm string, up *upload, contentType string, tags []string, expiresIn int64) (AssetRecord, error) {
	if p.baseDir == "" {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request",
			"Model-directory placement requires the proxy to run co-located "+
				"with ComfyUI (start it with --comfyui-base-dir).", nil)
		return AssetRecord{}, errResponded
	}
	if !looksLikeSafetensors(up.file, up.size) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request",
			"Model uploads must be valid safetensors files (header check failed).", nil)
		return AssetRecord{}, errResponded
	}
	// MODEL_ROOTS are the directories directly under ComfyUI's models/.
	dest, err := resolvePlacementPath(filepath.Join(p.baseDir, "models"), norm)
	if err != nil {
		var pe *errPlacement
		if errors.As(err, &pe) {
			httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", pe.Error(), nil)
			return AssetRecord{}, errResponded
		}
		return AssetRecord{}, err
	}
	if err := atomicNoClobberWrite(dest, up.file); err != nil {
		var pe *errPlacement
		if errors.As(err, &pe) {
			httpx.WriteError(w, http.StatusConflict, "hash_mismatch", pe.Error(), nil)
			return AssetRecord{}, errResponded
		}
		return AssetRecord{}, err
	}
	// ComfyUI's loaders name a model relative to its model root, without the
	// category segment, so that is what core/ASSET refs must resolve to.
	parts := pathParts(norm)
	rootRelative := strings.Join(parts[1:], "/")
	return p.assets.Add(AssetRecord{
		Hash:        up.hash,
		SizeBytes:   up.size,
		ContentType: contentType,
		FilePath:    &rootRelative,
		DiskPath:    &dest,
		Tags:        tags,
		ExpiresAt:   expiresAt(expiresIn),
	})
}

// uploadInput forwards a workflow input to ComfyUI's /upload/image, streaming
// it from the spooled temp file rather than holding it in memory.
func (p *Proxy) uploadInput(w http.ResponseWriter, r *http.Request, norm, origPath string, up *upload, contentType string, tags []string, expiresIn int64) (AssetRecord, error) {
	subfolder, filename := path.Split(norm)
	subfolder = strings.TrimSuffix(subfolder, "/")

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(writeUploadForm(mw, up.file, filename, contentType, subfolder))
	}()
	resp, err := p.comfy.PostMultipart(r.Context(), "/upload/image", mw.FormDataContentType(), pr)
	if closeErr := pr.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "upstream_error", "Failed to reach ComfyUI for upload.", nil)
		return AssetRecord{}, errResponded
	}
	if resp.Status != http.StatusOK {
		httpx.WriteError(w, http.StatusInternalServerError, "upstream_error", "ComfyUI rejected the upload.", nil)
		return AssetRecord{}, errResponded
	}
	var reply struct {
		Name      *string `json:"name"`
		Subfolder *string `json:"subfolder"`
		Type      *string `json:"type"`
		Asset     *struct {
			ID any `json:"id"`
		} `json:"asset"`
	}
	if err := json.Unmarshal(resp.Body, &reply); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "upstream_error", "Failed to reach ComfyUI for upload.", nil)
		return AssetRecord{}, errResponded
	}
	ref := ComfyRef{Filename: filename, Subfolder: subfolder, Type: "input"}
	if reply.Name != nil {
		ref.Filename = *reply.Name
	}
	if reply.Subfolder != nil {
		ref.Subfolder = *reply.Subfolder
	}
	if reply.Type != nil {
		ref.Type = *reply.Type
	}
	// ComfyUI with its asset catalog enabled hands back its own asset id;
	// reuse it so both sides agree on identity.
	assetID := ""
	if reply.Asset != nil {
		if s, ok := reply.Asset.ID.(string); ok {
			assetID = s
		}
	}
	return p.assets.Add(AssetRecord{
		ID:          assetID,
		Hash:        up.hash,
		SizeBytes:   up.size,
		ContentType: contentType,
		FilePath:    &origPath,
		ComfyRef:    &ref,
		Tags:        tags,
		ExpiresAt:   expiresAt(expiresIn),
	})
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func writeUploadForm(mw *multipart.Writer, src io.Reader, filename, contentType, subfolder string) error {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="image"; filename="`+quoteEscaper.Replace(filename)+`"`)
	h.Set("Content-Type", contentType)
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, src); err != nil {
		return err
	}
	if err := mw.WriteField("type", "input"); err != nil {
		return err
	}
	if subfolder != "" {
		if err := mw.WriteField("subfolder", subfolder); err != nil {
			return err
		}
	}
	return mw.Close()
}

func (p *Proxy) assetFromHash(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeJSON(http.MaxBytesReader(w, r.Body, 1<<20), &body); err != nil || body == nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "Body must be JSON.", nil)
		return
	}
	hash, ok := body["hash"].(string)
	if !ok {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Missing 'hash'.", nil)
		return
	}
	rec, found := p.assets.GetByHash(hash)
	if !found {
		// A miss and "exists but not yours" are deliberately identical.
		httpx.WriteError(w, http.StatusNotFound, "blob_not_found", "No blob the caller may mint from.", nil)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, assetJSON(rec, boolPtr(false), p.base(r)))
}

func (p *Proxy) headAssetByHash(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.assets.GetByHash(r.PathValue("hash")); ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (p *Proxy) getAsset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	base := p.base(r)
	if rec, ok := p.assets.Get(id); ok {
		httpx.WriteJSON(w, http.StatusOK, assetJSON(rec, nil, base))
		return
	}
	if ref, job, ok := p.decodeOutputAssetIDJob(id); ok {
		t := now()
		httpx.WriteJSON(w, http.StatusOK, assetBody{
			ID:           id,
			ContentType:  guessContentType(ref.Filename),
			SizeBytes:    p.outputSize(r.Context(), id, ref),
			FilePath:     strPtr(ref.Filename),
			CreatedAt:    iso(t),
			URL:          base + "/api/v2/assets/" + id + "/content",
			URLExpiresAt: iso(t.Add(retention)),
			JobID:        job,
		})
		return
	}
	httpx.WriteError(w, http.StatusNotFound, "not_found", "Unknown asset id.", nil)
}

func (p *Proxy) getAssetContent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, ok := p.assets.Get(id)
	if ok && rec.DiskPath != nil && *rec.DiskPath != "" {
		p.serveDiskFile(w, r, *rec.DiskPath)
		return
	}
	if ok && rec.ComfyRef != nil {
		p.streamView(w, r, *rec.ComfyRef)
		return
	}
	if ref, ok := p.decodeOutputAssetID(id); ok {
		p.streamView(w, r, ref)
		return
	}
	httpx.WriteError(w, http.StatusNotFound, "not_found", "Unknown asset id.", nil)
}

// serveDiskFile serves a proxy-placed or host-registered file with Range
// support.
func (p *Proxy) serveDiskFile(w http.ResponseWriter, r *http.Request, diskPath string) {
	f, err := os.Open(diskPath)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "output_unavailable",
			"The asset is registered but its bytes are no longer on disk. "+outputUnavailableMsg, nil)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			p.logf("error: close %s: %v", diskPath, err)
		}
	}()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		httpx.WriteError(w, http.StatusNotFound, "output_unavailable",
			"The asset is registered but its bytes are no longer on disk. "+outputUnavailableMsg, nil)
		return
	}
	if ct := mime.TypeByExtension(filepath.Ext(diskPath)); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// streamView proxies ComfyUI's /view, passing Range through and copying the
// body in chunks so a large output never sits in memory.
func (p *Proxy) streamView(w http.ResponseWriter, r *http.Request, ref ComfyRef) {
	typ := ref.Type
	if typ == "" {
		typ = "output"
	}
	q := url.Values{"filename": {ref.Filename}, "subfolder": {ref.Subfolder}, "type": {typ}}
	header := http.Header{}
	if rng := r.Header.Get("Range"); rng != "" {
		header.Set("Range", rng)
	}
	resp, err := p.comfy.Stream(r.Context(), "/view?"+q.Encode(), header)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "upstream_error", "Failed to reach ComfyUI for content.", nil)
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			p.logf("error: close /view body: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		if resp.StatusCode == http.StatusNotFound {
			httpx.WriteError(w, http.StatusNotFound, "output_unavailable",
				"Output bytes are no longer retrievable upstream. "+outputUnavailableMsg, nil)
			return
		}
		httpx.WriteError(w, http.StatusBadGateway, "upstream_error", "ComfyUI returned an unexpected status for the output.", nil)
		return
	}
	ct := resp.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		ct = mt
	} else {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		return // client or upstream went away mid-body; the status is sent
	}
}

func (p *Proxy) deleteAsset(w http.ResponseWriter, r *http.Request) {
	existed, err := p.assets.Delete(r.PathValue("id"))
	if err != nil {
		p.internalError(w, err)
		return
	}
	if !existed {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "Unknown asset id.", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// assetFromPath registers a file already under --comfyui-base-dir without
// copying it.
func (p *Proxy) assetFromPath(w http.ResponseWriter, r *http.Request) {
	if p.baseDir == "" {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request",
			"Host-path registration requires --comfyui-base-dir "+
				"(the proxy must share a filesystem with ComfyUI).", nil)
		return
	}
	var body map[string]any
	if err := decodeJSON(http.MaxBytesReader(w, r.Body, 1<<20), &body); err != nil || body == nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "Body must be JSON.", nil)
		return
	}
	pathRaw, _ := body["path"].(string)
	if pathRaw == "" {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Missing 'path' (absolute host path).", nil)
		return
	}
	var displayOverride string
	if fp, present := body["file_path"]; present && fp != nil {
		s, ok := fp.(string)
		if !ok {
			httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "'file_path' must be a string when provided.", nil)
			return
		}
		displayOverride = s
	}

	abs, err := filepath.Abs(pathRaw)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "blob_not_found", "Host path does not exist.", nil)
		return
	}
	src, err := filepath.EvalSymlinks(abs)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "blob_not_found", "Host path does not exist.", nil)
		return
	}
	info, err := os.Stat(src)
	if err != nil || !info.Mode().IsRegular() {
		httpx.WriteError(w, http.StatusNotFound, "blob_not_found", "Host path is not a regular file.", nil)
		return
	}
	rel, err := filepath.Rel(p.baseDir, src)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Host path must resolve under --comfyui-base-dir.", nil)
		return
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if rel == "." || len(parts) == 0 {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Host path must be a file under the install.", nil)
		return
	}
	if info.Size() > p.maxUpload {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("Host file exceeds %d bytes.", p.maxUpload), nil)
		return
	}

	hash, err := hashFile(src)
	if err != nil {
		p.internalError(w, err)
		return
	}
	base := p.base(r)
	if existing, ok := p.assets.GetByHash(hash); ok {
		httpx.WriteJSON(w, http.StatusOK, assetJSON(existing, boolPtr(false), base))
		return
	}
	contentType, isStr := body["content_type"].(string)
	if !isStr {
		contentType = guessContentType(src)
	}

	top := parts[0]
	if top == "models" {
		isModel, norm, err := validateUploadPath(strings.Join(parts, "/"))
		if err != nil {
			httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", err.Error(), nil)
			return
		}
		if !isModel {
			httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Invalid model path under models/.", nil)
			return
		}
		f, err := os.Open(src)
		if err != nil {
			p.internalError(w, err)
			return
		}
		ok := looksLikeSafetensors(f, info.Size())
		if err := f.Close(); err != nil {
			p.internalError(w, err)
			return
		}
		if !ok {
			httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Model registrations must be valid safetensors files.", nil)
			return
		}
		filePath := strings.Join(pathParts(norm)[1:], "/")
		if displayOverride != "" {
			filePath = displayOverride
		}
		rec, err := p.assets.Add(AssetRecord{Hash: hash, SizeBytes: info.Size(), ContentType: contentType, FilePath: &filePath, DiskPath: &src})
		if err != nil {
			p.internalError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, assetJSON(rec, boolPtr(true), base))
		return
	}

	if top != "input" && top != "output" && top != "temp" {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Host path must be under input/, output/, temp/, or models/.", nil)
		return
	}
	remainder := parts[1:]
	if len(remainder) == 0 {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", "Host path must include a filename.", nil)
		return
	}
	display := strings.Join(parts, "/")
	if displayOverride != "" {
		display = displayOverride
	}
	ref := ComfyRef{Filename: remainder[len(remainder)-1], Subfolder: strings.Join(remainder[:len(remainder)-1], "/"), Type: top}
	rec, err := p.assets.Add(AssetRecord{Hash: hash, SizeBytes: info.Size(), ContentType: contentType, FilePath: &display, ComfyRef: &ref, DiskPath: &src})
	if err != nil {
		p.internalError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, assetJSON(rec, boolPtr(true), base))
}

func hashFile(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", name, err)
	}
	h := blake3.New(32, nil)
	_, copyErr := io.Copy(h, f)
	if err := errors.Join(copyErr, f.Close()); err != nil {
		return "", fmt.Errorf("hash %s: %w", name, err)
	}
	return "blake3:" + hex.EncodeToString(h.Sum(nil)), nil
}
