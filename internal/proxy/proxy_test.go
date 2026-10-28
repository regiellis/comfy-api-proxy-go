package proxy

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testProxy(t *testing.T) *Proxy {
	t.Helper()
	return &Proxy{secret: []byte("test-secret"), assets: NewAssetStore(nil), jobs: newJobStore(nil), idem: newIdempotency(nil)}
}

func TestOutputAssetIDRoundTrip(t *testing.T) {
	p := testProxy(t)
	id := p.outputAssetID("job-a", "out.png", "sub", "output")
	ref, job, ok := p.decodeOutputAssetIDJob(id)
	if !ok || ref != (ComfyRef{"out.png", "sub", "output"}) || job == nil || *job != "job-a" {
		t.Fatalf("decode = %+v, %v, %v", ref, job, ok)
	}
	if other := p.outputAssetID("job-b", "out.png", "sub", "output"); other == id {
		t.Fatal("two jobs with the same filename share an asset id")
	}
}

// An id minted by the Python reference (its json.dumps puts spaces after the
// separators) must verify against the same secret, so a --state-dir shared
// with the Python proxy keeps its output links working.
func TestOutputAssetIDFromReference(t *testing.T) {
	secret, err := hex.DecodeString("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{secret: secret}
	const minted = "eyJqIjogImpvYi0xIiwgImYiOiAib3V0LnBuZyIsICJzIjogInN1YiIsICJ0IjogIm91dHB1dCJ9.hfY9cev7zm5tx5k2_FfbC5A0g5K9UjXHJcfUII4qWzw"
	ref, job, ok := p.decodeOutputAssetIDJob(minted)
	if !ok || ref != (ComfyRef{"out.png", "sub", "output"}) || job == nil || *job != "job-1" {
		t.Fatalf("decode = %+v, %v, %v", ref, job, ok)
	}

	// Go mints the identical id, escapes included, so a state directory
	// moved from the Python proxy keeps every output's id.
	vectors := []struct {
		job, file, sub, typ, want string
	}{
		{"job-1", "out.png", "sub", "output", minted},
		{"j", "naïve \"q\"\\\n\t😀\x7f.png", "a/b", "temp",
			"eyJqIjogImoiLCAiZiI6ICJuYVx1MDBlZnZlIFwicVwiXFxcblx0XHVkODNkXHVkZTAwXHUwMDdmLnBuZyIsICJzIjogImEvYiIsICJ0IjogInRlbXAifQ.ndrTNRC9lPxtIyPKZb-OV-8fBRmpiK1HI1EGOi4flPg"},
	}
	for _, v := range vectors {
		if got := p.outputAssetID(v.job, v.file, v.sub, v.typ); got != v.want {
			t.Errorf("outputAssetID(%q) = %s, want %s", v.file, got, v.want)
		}
	}
}

func TestOutputAssetIDRejectsForgeries(t *testing.T) {
	p := testProxy(t)
	id := p.outputAssetID("job", "out.png", "", "output")
	payload, _, _ := strings.Cut(id, ".")

	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"j":"job","f":"../../etc/passwd","s":"","t":"output"}`))
	other := &Proxy{secret: []byte("another-secret")}
	for name, bad := range map[string]string{
		"no dot":         payload,
		"wrong tag":      payload + ".AAAA",
		"forged payload": forged + "." + strings.SplitN(id, ".", 2)[1],
		"other secret":   other.outputAssetID("job", "out.png", "", "output"),
		"uuid":           "11111111-1111-1111-1111-111111111111",
	} {
		if _, ok := p.decodeOutputAssetID(bad); ok {
			t.Errorf("%s decoded", name)
		}
	}
}

func TestValidJobID(t *testing.T) {
	for _, ok := range []string{
		"11111111-1111-1111-1111-111111111111",
		"11111111111111111111111111111111",
		"{11111111-1111-1111-1111-111111111111}",
		"urn:uuid:11111111-1111-1111-1111-111111111111",
		"ABCDEFAB-1111-1111-1111-111111111111",
	} {
		if !validJobID(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "x", "../../prompt", "11111111-1111-1111-1111-11111111111?", "11111111-1111-1111-1111-1111111111/1"} {
		if validJobID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNewUUIDIsV4(t *testing.T) {
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) || !validJobID(id) {
		t.Fatalf("id = %s", id)
	}
}

func TestParseSubmitExtras(t *testing.T) {
	decode := func(s string) map[string]any {
		var m map[string]any
		if err := decodeJSON(strings.NewReader(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	ex, msg := parseSubmitExtras(decode(`{"metadata":"batch-7","priority":-5}`))
	if msg != "" || *ex.metadata != "batch-7" || *ex.priority != -5 {
		t.Fatalf("got %+v %q", ex, msg)
	}
	for body, want := range map[string]string{
		`{"metadata":5}`: "must be a string",
		`{"metadata":"` + strings.Repeat("é", 513) + `"}`: "exceeds 1024",
		`{"priority":1.5}`:                     "must be an integer",
		`{"priority":1e3}`:                     "must be an integer",
		`{"priority":true}`:                    "must be an integer",
		`{"priority":"3"}`:                     "must be an integer",
		`{"priority":1000001}`:                 "between",
		`{"priority":99999999999999999999999}`: "between",
	} {
		if _, msg := parseSubmitExtras(decode(body)); !strings.Contains(msg, want) {
			t.Errorf("%s: %q, want %q", body, msg, want)
		}
	}
	if _, msg := parseSubmitExtras(decode(`{"metadata":null,"priority":null}`)); msg != "" {
		t.Errorf("nulls rejected: %q", msg)
	}
}

func TestDecodeJSONKeepsLargeSeeds(t *testing.T) {
	var m map[string]any
	if err := decodeJSON(strings.NewReader(`{"seed":18446744073709551615}`), &m); err != nil {
		t.Fatal(err)
	}
	out, err := encodeJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"seed":18446744073709551615}` {
		t.Fatalf("seed mangled: %s", out)
	}
}

func TestRewriteAssetRefs(t *testing.T) {
	p := testProxy(t)
	fp := "photo.png"
	rec, err := p.assets.Add(AssetRecord{Hash: "blake3:aa", FilePath: &fp, ComfyRef: &ComfyRef{Filename: "photo.png", Subfolder: "sub", Type: "input"}})
	if err != nil {
		t.Fatal(err)
	}
	out := p.outputAssetID("j", "o.png", "", "output")
	var wf map[string]any
	src := `{"1":{"inputs":{"a":{"__type":"core/ASSET","info":{"id":"` + rec.ID + `"}},
		"b":{"__type":"core/ASSET","info":{"hash":"blake3:aa"}},
		"c":{"__type":"core/ASSET","info":{"id":"` + out + `"}},
		"d":[{"__type":"core/ASSET","info":{"id":"nope"}}],
		"e":{"__type":"core/ASSET"},
		"f":{"__type":"core/ASSET","info":{"id":7}}}}}`
	if err := decodeJSON(strings.NewReader(src), &wf); err != nil {
		t.Fatal(err)
	}
	var missing []string
	got := p.rewriteAssetRefs(wf, &missing)
	inputs := got.(map[string]any)["1"].(map[string]any)["inputs"].(map[string]any)
	if inputs["a"] != "sub/photo.png" || inputs["b"] != "sub/photo.png" || inputs["c"] != "o.png" {
		t.Fatalf("resolved = %v", inputs)
	}
	want := map[string]bool{"nope": true, "<malformed core/ASSET: missing info>": true, "7": true}
	if len(missing) != 3 {
		t.Fatalf("missing = %v", missing)
	}
	for _, m := range missing {
		if !want[m] {
			t.Errorf("unexpected missing id %q", m)
		}
	}
}

func TestIsUIFormat(t *testing.T) {
	if !isUIFormat(map[string]any{"nodes": []any{}, "links": []any{}}) {
		t.Error("UI export not detected")
	}
	if isUIFormat(map[string]any{"1": map[string]any{"class_type": "X"}}) {
		t.Error("API format flagged as UI")
	}
}

func TestObjectEntriesKeepsOrder(t *testing.T) {
	es, err := objectEntries(json.RawMessage(`{"9":1,"10":2,"2":3}`))
	if err != nil || len(es) != 3 || es[0].key != "9" || es[1].key != "10" || es[2].key != "2" {
		t.Fatalf("entries = %v, %v", es, err)
	}
}

func TestTerminalStateMapping(t *testing.T) {
	p := testProxy(t)
	parse := func(s string) *historyEntry {
		var e historyEntry
		if err := json.Unmarshal([]byte(s), &e); err != nil {
			t.Fatal(err)
		}
		return &e
	}
	ok := p.terminalState("j", parse(`{"outputs":{"9":{"images":[{"filename":"a.png","subfolder":"","type":"output"}],"unknown":[{"filename":"x"}]},
		"3":{"text":[{"filename":"t.txt"}]}},"status":{"status_str":"success","messages":[["execution_cached",{"nodes":[]}]]}}`), "http://h")
	if ok.Status != "succeeded" || ok.OutputsReused || len(ok.Outputs) != 2 {
		t.Fatalf("success = %+v", ok)
	}
	if ok.Outputs[0].NodeID != "9" || ok.Outputs[0].ContentType != "image/png" || ok.Outputs[1].Type != "text" || ok.Outputs[1].ContentType != "text/plain" {
		t.Fatalf("outputs = %+v", ok.Outputs)
	}
	if !strings.HasPrefix(ok.Outputs[0].URL, "http://h/api/v2/assets/") {
		t.Fatalf("url = %s", ok.Outputs[0].URL)
	}

	cached := p.terminalState("j", parse(`{"outputs":{},"status":{"messages":[["execution_cached",{"nodes":["1"]}]]}}`), "")
	if cached.Status != "succeeded" || !cached.OutputsReused {
		t.Fatalf("cached = %+v", cached)
	}
	canceled := p.terminalState("j", parse(`{"status":{"status_str":"error","messages":[["execution_interrupted",{}]]}}`), "")
	if canceled.Status != "canceled" {
		t.Fatalf("canceled = %+v", canceled)
	}
	failed := p.terminalState("j", parse(`{"status":{"status_str":"error","messages":[["execution_error",{"exception_message":"boom","node_id":"4","node_type":"KSampler"}]]}}`), "")
	if failed.Status != "failed" || failed.Error.Message != "boom" || failed.Error.NodeID != "4" || failed.Error.ClassType != "KSampler" {
		t.Fatalf("failed = %+v %+v", failed, failed.Error)
	}
	bare := p.terminalState("j", parse(`{"status":{"status_str":"error","messages":[]}}`), "")
	if bare.Error.Message != "execution failed" || bare.Error.NodeID != nil {
		t.Fatalf("bare failure = %+v", bare.Error)
	}
}

func TestQueueItemID(t *testing.T) {
	if id, ok := queueItemID(json.RawMessage(`[0,"abc",{}]`)); !ok || id != "abc" {
		t.Errorf("got %q %v", id, ok)
	}
	if _, ok := queueItemID(json.RawMessage(`[0]`)); ok {
		t.Error("short item accepted")
	}
	if id, ok := queueItemID(json.RawMessage(`[0,5]`)); !ok || id != "" {
		t.Errorf("non-string id: %q %v", id, ok)
	}
}

func previewFrame(kind, word uint32, rest []byte) []byte {
	b := make([]byte, 8, 8+len(rest))
	binary.BigEndian.PutUint32(b[:4], kind)
	binary.BigEndian.PutUint32(b[4:8], word)
	return append(b, rest...)
}

func TestPreviewDecoding(t *testing.T) {
	img := []byte("\x89PNG\r\n\x1a\nbody")
	meta := []byte(`{"image_type":"image/webp","node_id":"7"}`)
	cases := []struct {
		name, ct, node string
		frame          []byte
		img            []byte
	}{
		{"with metadata", "image/webp", "7", previewFrame(binPreviewMeta, uint32(len(meta)), append(append([]byte{}, meta...), img...)), img},
		{"legacy png", "image/png", "", previewFrame(binPreview, previewPNG, img), img},
		{"legacy jpeg", "image/jpeg", "", previewFrame(binPreview, previewJPEG, img), img},
		{"bad metadata", "image/jpeg", "", previewFrame(binPreviewMeta, 9, append([]byte("{not-json"), 'I', 'M', 'G')), []byte("IMG")},
	}
	for _, tc := range cases {
		b := &bridge{}
		ev, ok := b.preview(tc.frame)
		if !ok {
			t.Fatalf("%s: not decoded", tc.name)
		}
		raw, err := base64.StdEncoding.DecodeString(ev.DataBase64)
		if err != nil || ev.ContentType != tc.ct || ev.NodeID != tc.node || string(raw) != string(tc.img) {
			t.Errorf("%s: %+v (%q)", tc.name, ev, raw)
		}
	}
	b := &bridge{}
	if _, ok := b.preview([]byte{0, 0, 0, 1}); ok {
		t.Error("short frame decoded")
	}
	if _, ok := b.preview(previewFrame(9, 0, nil)); ok {
		t.Error("unknown event type decoded")
	}
}

func TestPreviewThrottle(t *testing.T) {
	b := &bridge{}
	frame := previewFrame(binPreview, previewPNG, []byte("x"))
	if _, ok := b.preview(frame); !ok {
		t.Fatal("first preview dropped")
	}
	if _, ok := b.preview(frame); ok {
		t.Fatal("second preview inside the window was not throttled")
	}
	b.lastPreview = time.Now().Add(-throttle)
	if _, ok := b.preview(frame); !ok {
		t.Fatal("preview after the window dropped")
	}
}

func progressData(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTranslateProgress(t *testing.T) {
	step := translateProgress("progress", progressData(t, `{"value":3,"max":12,"node":"5"}`))
	if step.Value != 0.25 || *step.Step != 3 || *step.Steps != 12 || *step.CurrentNode != "5" {
		t.Fatalf("progress = %+v", step)
	}
	zeroMax := translateProgress("progress", progressData(t, `{"value":3,"max":0}`))
	if zeroMax.Value != 1 || zeroMax.CurrentNode != nil {
		t.Fatalf("zero max = %+v", zeroMax)
	}

	empty := translateProgress("progress_state", progressData(t, `{"nodes":{}}`))
	if empty.Value != 0 || empty.NodesTotal != 0 {
		t.Fatalf("empty = %+v", empty)
	}
	all := translateProgress("progress_state", progressData(t, `{"nodes":{"a":{"state":"finished"},"b":{"state":"finished"}}}`))
	if all.Value != 1 || all.NodesDone != 2 {
		t.Fatalf("all done = %+v", all)
	}
	mixed := translateProgress("progress_state", progressData(t, `{"nodes":{"a":{"state":"finished"},"b":{"state":"running","value":3,"max":10},"c":{"state":"pending"},"d":{"state":"pending"}}}`))
	if math.Abs(mixed.Value-(1+0.3)/4) > 1e-9 || *mixed.CurrentNode != "b" || *mixed.Step != 3 || *mixed.Steps != 10 {
		t.Fatalf("mixed = %+v", mixed)
	}
	zero := translateProgress("progress_state", progressData(t, `{"nodes":{"a":{"state":"finished"},"b":{"state":"running","value":0,"max":0}}}`))
	if zero.Value != 0.5 {
		t.Fatalf("running node with max 0 = %+v", zero)
	}
}

func TestForThisJob(t *testing.T) {
	b := &bridge{jobID: "mine"}
	cases := map[string]bool{
		`{"type":"progress","data":{"prompt_id":"mine"}}`:  true,
		`{"type":"progress","data":{"prompt_id":"other"}}`: false,
		`{"type":"status","data":{}}`:                      true,
		`{"type":"status"}`:                                true,
		`[1,2,3]`:                                          false,
		`not json`:                                         false,
		`{"type":"progress","data":[1]}`:                   true,
	}
	for raw, want := range cases {
		if _, _, got := b.forThisJob([]byte(raw)); got != want {
			t.Errorf("%s: %v, want %v", raw, got, want)
		}
	}
}

func TestAssetStoreFirstWriterWinsHash(t *testing.T) {
	s := NewAssetStore(nil)
	if _, ok := s.Get("nope"); ok {
		t.Fatal("empty store returned a record")
	}
	a, err := s.Add(AssetRecord{Hash: "blake3:x"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Add(AssetRecord{Hash: "blake3:x"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetByHash("blake3:x"); got.ID != a.ID {
		t.Fatalf("hash slot owner = %s, want first writer %s", got.ID, a.ID)
	}
	if strings.Contains(a.ID, ".") || !validJobID(a.ID) {
		t.Fatalf("asset id %q is not a bare uuid", a.ID)
	}
	if ok, err := s.Delete(a.ID); !ok || err != nil {
		t.Fatalf("delete = %v %v", ok, err)
	}
	if _, ok := s.GetByHash("blake3:x"); ok {
		t.Fatal("hash slot survived its owner's delete")
	}
	if _, ok := s.Get(b.ID); !ok {
		t.Fatal("second record lost")
	}
}

func TestAssetStoreDeleteExpired(t *testing.T) {
	s := NewAssetStore(nil)
	past, future, bad := iso(now().Add(-time.Minute)), iso(now().Add(time.Hour)), "garbage"
	gone, err := s.Add(AssetRecord{ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []*string{&future, &bad, nil} {
		if _, err := s.Add(AssetRecord{ExpiresAt: e}); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := s.DeleteExpired(now())
	if err != nil || len(deleted) != 1 || deleted[0] != gone.ID {
		t.Fatalf("deleted = %v, %v", deleted, err)
	}
}

func TestIdempotencyEviction(t *testing.T) {
	s := newIdempotency(nil)
	for i := 0; i <= maxIdempotencyKeys; i++ {
		if ok, err := s.claim("k" + strconv.Itoa(i)); !ok || err != nil {
			t.Fatalf("claim %d = %v %v", i, ok, err)
		}
	}
	if ok, _ := s.claim("k0"); !ok {
		t.Fatal("oldest key was not evicted past the cap")
	}
	if ok, _ := s.claim("k5"); ok {
		t.Fatal("a held key was claimable twice")
	}
	if err := s.release("k5"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.claim("k5"); !ok {
		t.Fatal("released key not claimable")
	}
}

func TestBaseUsesPublicURL(t *testing.T) {
	p := testProxy(t)
	r := httptest.NewRequest(http.MethodGet, "http://10.0.0.5:8189/api/v2/jobs", nil)
	r.Host = "10.0.0.5:8189"
	if got := p.base(r); got != "http://10.0.0.5:8189" {
		t.Fatalf("without public_url, base = %q", got)
	}
	// Behind a TLS-terminating proxy the request arrives as plain http; the
	// links must still carry the public origin, or clients drop the token.
	p.publicURL = "https://comfy.example.com"
	if got := p.base(r); got != "https://comfy.example.com" {
		t.Fatalf("with public_url, base = %q", got)
	}
}
