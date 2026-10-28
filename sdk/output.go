package sdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Output is one file a job produced.
type Output struct {
	t *transport
	w wireOutput
}

// NodeID is the node that produced the output.
func (o *Output) NodeID() string { return o.w.NodeID }

// Name is the output's filename.
func (o *Output) Name() string { return o.w.Name }

// Type is image, video, audio, text, file, or latent.
func (o *Output) Type() string { return o.w.Type }

// ID is the output's asset id.
func (o *Output) ID() string { return o.w.ID }

// SizeBytes is the size the server reported, zero when it did not know.
func (o *Output) SizeBytes() int64 { return o.w.SizeBytes }

// ContentType is the output's media type.
func (o *Output) ContentType() string { return o.w.ContentType }

// JobID is the job that produced the output.
func (o *Output) JobID() *string { return o.w.JobID }

// URLExpiresAt is when the output's content URL stops working.
func (o *Output) URLExpiresAt() time.Time { return o.w.URLExpiresAt }

// String reports node id, name, and asset id.
func (o *Output) String() string {
	return fmt.Sprintf("Output(node_id=%q, name=%q, id=%q)", o.w.NodeID, o.w.Name, o.w.ID)
}

// Range is an inclusive byte range for a partial download.
type Range struct{ Start, End int64 }

func (o *Output) open(ctx context.Context, rng *Range) (io.ReadCloser, error) {
	var header http.Header
	if rng != nil {
		header = http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", rng.Start, rng.End)}}
	}
	resp, err := o.t.stream(ctx, o.t.http, http.MethodGet, "/assets/"+url.PathEscape(o.w.ID)+"/content",
		header, o.t.idle, http.StatusOK, http.StatusPartialContent)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ToWriter streams the output (or rng of it, when non-nil) into w and
// reports the bytes written.
func (o *Output) ToWriter(ctx context.Context, w io.Writer, rng *Range) (int64, error) {
	body, err := o.open(ctx, rng)
	if err != nil {
		return 0, err
	}
	tw := &trackedWriter{w: w}
	n, copyErr := io.Copy(tw, body)
	closeErr := body.Close()
	return n, o.copyError(copyErr, tw.err, closeErr)
}

// trackedWriter remembers a write failure, so a full disk or a broken
// writer is reported as itself rather than as a network failure, which
// would tell the caller to poll before retrying.
type trackedWriter struct {
	w   io.Writer
	err error
}

func (t *trackedWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil {
		t.err = err
	}
	return n, err
}

func (o *Output) copyError(copyErr, writeErr, closeErr error) error {
	if copyErr == nil && closeErr == nil {
		return nil
	}
	if writeErr != nil && errors.Is(copyErr, writeErr) {
		return errors.Join(fmt.Errorf("write %s: %w", o.w.Name, writeErr), closeErr)
	}
	var te *TransportError
	if errors.As(copyErr, &te) {
		return errors.Join(copyErr, closeErr)
	}
	return &TransportError{Err: fmt.Errorf("download %s: %w", o.w.Name, errors.Join(copyErr, closeErr))}
}

// ToFile streams the output to path, creating or truncating it. A failed
// download removes the partial file.
func (o *Output) ToFile(ctx context.Context, path string, rng *Range) error {
	body, err := o.open(ctx, rng)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return errors.Join(fmt.Errorf("create %s: %w", path, err), body.Close())
	}
	tw := &trackedWriter{w: f}
	_, copyErr := io.Copy(tw, body)
	fileErr := f.Close()
	if err := o.copyError(copyErr, tw.err, body.Close()); err != nil || fileErr != nil {
		if fileErr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", path, fileErr))
		}
		return errors.Join(err, removeQuiet(path))
	}
	return nil
}

func removeQuiet(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Bytes reads the whole output (or rng of it) into memory.
func (o *Output) Bytes(ctx context.Context, rng *Range) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := o.ToWriter(ctx, &buf, rng); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DownloadURL resolves a URL for the output's bytes: a signed URL with its
// expiry on Comfy Cloud, the content endpoint itself on a self-hosted proxy.
func (o *Output) DownloadURL(ctx context.Context) (DownloadURL, error) {
	return o.t.contentURL(ctx, o.w.ID)
}
