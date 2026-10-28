package state

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/regiellis/comfy-api-proxy-go/internal/proxy"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestOwnerOnlyPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	open(t, dir)
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "state.sqlite3"): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}

func TestJobsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	meta, prio := "label", int64(-3)
	base := time.Date(2026, 10, 1, 12, 0, 0, 123456000, time.UTC)
	for i, id := range []string{"a", "b", "c"} {
		rec := proxy.JobRecord{ID: id, CreatedAt: base.Add(time.Duration(i) * time.Minute), ClientID: id, Workflow: []byte(`{"1":{}}`)}
		if id == "b" {
			rec.Metadata, rec.Priority = &meta, &prio
		}
		if err := s.UpsertJob(rec); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := s.LoadJobs(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].ID != "b" || recs[1].ID != "c" {
		t.Fatalf("LoadJobs(2) = %+v, want newest two oldest first", recs)
	}
	if recs[0].Workflow != nil || *recs[0].Metadata != "label" || *recs[0].Priority != -3 {
		t.Fatalf("record b = %+v", recs[0])
	}
	if !recs[0].CreatedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("created_at = %v", recs[0].CreatedAt)
	}
	got, err := s.GetJob("a")
	if err != nil || got == nil || got.Metadata != nil {
		t.Fatalf("GetJob(a) = %+v, %v", got, err)
	}
	if missing, err := s.GetJob("zzz"); missing != nil || err != nil {
		t.Fatalf("GetJob(missing) = %+v, %v", missing, err)
	}
	wf, err := s.GetJobWorkflow("a")
	if err != nil || string(wf) != `{"1":{}}` {
		t.Fatalf("workflow = %s, %v", wf, err)
	}
}

func TestIdempotency(t *testing.T) {
	s := open(t, t.TempDir())
	at := time.Now()
	if ok, err := s.ClaimIdempotency("k1", at); !ok || err != nil {
		t.Fatalf("first claim = %v %v", ok, err)
	}
	if ok, err := s.ClaimIdempotency("k1", at); ok || err != nil {
		t.Fatalf("second claim = %v %v", ok, err)
	}
	if ok, err := s.ClaimIdempotency("k2", at.Add(time.Second)); !ok || err != nil {
		t.Fatalf("k2 = %v %v", ok, err)
	}
	if ok, err := s.ClaimIdempotency("k3", at.Add(2*time.Second)); !ok || err != nil {
		t.Fatalf("k3 = %v %v", ok, err)
	}
	if err := s.TrimIdempotency(2); err != nil {
		t.Fatal(err)
	}
	keys, err := s.LoadIdempotencyKeys()
	if err != nil || len(keys) != 2 || keys[0] != "k2" || keys[1] != "k3" {
		t.Fatalf("keys after trim = %v, %v", keys, err)
	}
	if err := s.ReleaseIdempotency("k2"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimIdempotency("k2", at); !ok || err != nil {
		t.Fatalf("reclaim after release = %v %v", ok, err)
	}
}

func TestAssetsRoundTrip(t *testing.T) {
	s := open(t, t.TempDir())
	fp, exp, disk := "x.png", "2026-10-02T00:00:00.000000+00:00", "/tmp/x"
	a := proxy.AssetRecord{ID: "a", Hash: "blake3:h", SizeBytes: 4, ContentType: "image/png", FilePath: &fp,
		CreatedAt: "2026-10-01T00:00:00.000000+00:00", ExpiresAt: &exp, ComfyRef: &proxy.ComfyRef{Filename: "x.png", Type: "input"}, Tags: []string{"t"}}
	b := proxy.AssetRecord{ID: "b", Hash: "blake3:h", ContentType: "x", CreatedAt: a.CreatedAt, DiskPath: &disk}
	for _, r := range []proxy.AssetRecord{a, b} {
		if err := s.UpsertAsset(r); err != nil {
			t.Fatal(err)
		}
	}
	recs, idx, err := s.LoadAssets()
	if err != nil || len(recs) != 2 || idx["blake3:h"] != "a" {
		t.Fatalf("load = %+v %v %v", recs, idx, err)
	}
	for _, r := range recs {
		if r.ID == "a" && (*r.ExpiresAt != exp || r.ComfyRef.Filename != "x.png" || r.Tags[0] != "t" || r.DiskPath != nil) {
			t.Fatalf("record a = %+v", r)
		}
	}
	if err := s.DeleteAsset("a"); err != nil {
		t.Fatal(err)
	}
	recs, idx, err = s.LoadAssets()
	if err != nil || len(recs) != 1 || len(idx) != 0 {
		t.Fatalf("after delete = %+v %v %v", recs, idx, err)
	}
}

func TestMeta(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, ok, err := s.GetMeta("asset_secret"); ok || err != nil {
		t.Fatalf("empty meta = %v %v", ok, err)
	}
	for _, v := range []string{"one", "two"} {
		if err := s.SetMeta("asset_secret", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok, err := s.GetMeta("asset_secret"); !ok || err != nil || v != "two" {
		t.Fatalf("meta = %q %v %v", v, ok, err)
	}
}

// A state directory written by the Python proxy has no assets.expires_at.
// Opening it must add the column and keep the rows.
func TestOpensReferenceSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := `CREATE TABLE assets (id TEXT PRIMARY KEY, hash TEXT NOT NULL, size_bytes INTEGER NOT NULL,
		content_type TEXT NOT NULL, file_path TEXT, created_at TEXT NOT NULL, comfy_ref TEXT, disk_path TEXT,
		tags TEXT NOT NULL DEFAULT '[]');
		INSERT INTO assets VALUES ('old','blake3:o',1,'image/png','o.png','2026-01-01T00:00:00+00:00',
		'{"filename":"o.png","subfolder":"","type":"input"}',NULL,'[]');
		CREATE TABLE jobs (id TEXT PRIMARY KEY, created_at TEXT NOT NULL, client_id TEXT NOT NULL,
		metadata TEXT, priority INTEGER, workflow TEXT NOT NULL);
		INSERT INTO jobs VALUES ('j','2026-01-01T00:00:00.5+00:00','j',NULL,NULL,'{}');`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := open(t, dir)
	recs, _, err := s.LoadAssets()
	if err != nil || len(recs) != 1 || recs[0].ExpiresAt != nil || recs[0].ComfyRef == nil {
		t.Fatalf("legacy assets = %+v, %v", recs, err)
	}
	jobs, err := s.LoadJobs(10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("legacy jobs = %+v, %v", jobs, err)
	}
}
