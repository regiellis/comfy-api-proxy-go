package proxy

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// uploadRequest builds a multipart upload. fields are written in order, and
// the file part comes last.
func uploadRequest(t *testing.T, fields [][2]string, file string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if file != "" {
		part, err := mw.CreateFormFile("file", "x.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(file)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v2/assets", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func uploadProxy(t *testing.T) *Proxy {
	t.Helper()
	p := testProxy(t)
	p.maxUpload = 1 << 20
	return p
}

func TestUploadRejectsBeforeUpstream(t *testing.T) {
	cases := []struct {
		name   string
		fields [][2]string
		want   string
	}{
		{"expires_in past year 9999", [][2]string{{"file_path", "x.png"}, {"expires_in", "10000000000000"}}, "'expires_in' is out of range"},
		{"expires_in past int64", [][2]string{{"file_path", "x.png"}, {"expires_in", "99999999999999999999999"}}, "'expires_in' is out of range"},
		{"CRLF in file_path", [][2]string{{"file_path", "a\r\nX-Injected: 1\r\nb.png"}}, "'file_path' must not contain control characters"},
		{"CRLF in content_type", [][2]string{{"file_path", "x.png"}, {"content_type", "image/png\r\nX: y"}}, "'content_type' must not contain control characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			uploadProxy(t).uploadAsset(rec, uploadRequest(t, tc.fields, "bytes"))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestUploadPartCap(t *testing.T) {
	fields := [][2]string{{"file_path", "x.png"}}
	for i := 0; i < maxUploadParts; i++ {
		fields = append(fields, [2]string{"tags", "t" + strconv.Itoa(i)})
	}
	rec := httptest.NewRecorder()
	uploadProxy(t).uploadAsset(rec, uploadRequest(t, fields, "bytes"))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "more than") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestExpiresAtFarFutureDoesNotWrap(t *testing.T) {
	// About 317 years: past time.Duration's range, inside year 9999.
	got := expiresAt(10_000_000_000)
	when, err := parseISO(*got)
	if err != nil {
		t.Fatal(err)
	}
	if !when.After(time.Now().AddDate(300, 0, 0)) {
		t.Fatalf("expires_at = %s, want about 317 years out", *got)
	}
	if expiresAt(0) != nil {
		t.Fatal("zero should mean no expiry")
	}
	if !validExpiresIn(-60) || validExpiresIn(1<<62) {
		t.Fatal("range check wrong")
	}
}
