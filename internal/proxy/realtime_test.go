package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/regiellis/comfy-api-proxy-go/internal/comfy"
)

const jobID = "11111111-1111-1111-1111-111111111111"

// fakeComfy answers /queue with the job running until historyAfter calls of
// /history, then reports it succeeded. Its /ws never sends the job's own
// messages, only the broadcast noise a real ComfyUI with custom nodes emits.
func fakeComfy(t *testing.T, historyAfter int32, noise bool, wsUp bool) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /history/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{}`
		if calls.Add(1) > historyAfter {
			body = `{"` + jobID + `":{"outputs":{"9":{"images":[{"filename":"o.png","subfolder":"","type":"output"}]}},"status":{"status_str":"success","messages":[]}}}`
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Error(err)
		}
	})
	mux.HandleFunc("GET /queue", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"queue_running":[[0,"` + jobID + `"]],"queue_pending":[]}`)); err != nil {
			t.Error(err)
		}
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		if !wsUp {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() {
			if err := c.CloseNow(); err != nil {
				return // the bridge already hung up
			}
		}()
		for {
			if noise {
				if err := c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"crystools.monitor","data":{"cpu_utilization":3}}`)); err != nil {
					return
				}
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fastStream(t *testing.T) {
	t.Helper()
	oldBeat, oldPoll := heartbeat, pollInterval
	heartbeat, pollInterval = 50*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { heartbeat, pollInterval = oldBeat, oldPoll })
}

func streamEvents(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	p := &Proxy{comfy: comfy.New(srv.URL, ""), secret: []byte("s"), jobs: newJobStore(nil), assets: NewAssetStore(nil), idem: newIdempotency(nil)}
	p.streamsCtx, p.stopStreams = context.WithCancel(context.Background())
	if err := p.jobs.remember(JobRecord{ID: jobID, CreatedAt: now(), ClientID: jobID}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	p.Register(mux)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v2/jobs/"+jobID+"/events", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		<-done
		t.Fatalf("stream did not end on its own; got:\n%s", rec.Body.String())
	}
	return rec.Body.String()
}

func lastStatus(t *testing.T, body string) string {
	t.Helper()
	status := ""
	for _, block := range strings.Split(body, "\n\n") {
		if !strings.HasPrefix(block, "event: status\n") {
			continue
		}
		var ev struct{ Status string }
		if err := json.Unmarshal([]byte(strings.TrimPrefix(block, "event: status\ndata: ")), &ev); err != nil {
			t.Fatal(err)
		}
		status = ev.Status
	}
	return status
}

// The job finishes between the first snapshot and the WebSocket connect, so
// its terminal message goes to nobody, while broadcast frames keep arriving.
// The stream must still end on the terminal status.
func TestStreamEndsWhenTerminalMessageWasMissedUnderBroadcastNoise(t *testing.T) {
	fastStream(t)
	// Only the snapshot taken right after connecting can see the job is done:
	// the heartbeat is too slow to rescue it inside the test's deadline.
	heartbeat = time.Hour
	// /history call 1 is the handler's 404 check, call 2 the bridge's first
	// snapshot, both still running; call 3, after the connect, sees it done.
	body := streamEvents(t, fakeComfy(t, 2, true, true))
	if got := lastStatus(t, body); got != "succeeded" {
		t.Fatalf("last status %q; body:\n%s", got, body)
	}
}

// The job finishes later, while noise flows and no terminal message comes:
// only the fixed heartbeat can notice.
func TestHeartbeatReconcilesDespiteConstantFrames(t *testing.T) {
	fastStream(t)
	body := streamEvents(t, fakeComfy(t, 3, true, true))
	if got := lastStatus(t, body); got != "succeeded" {
		t.Fatalf("last status %q; body:\n%s", got, body)
	}
}

func TestStreamFallsBackToPollingWhenWebSocketUnreachable(t *testing.T) {
	fastStream(t)
	body := streamEvents(t, fakeComfy(t, 3, false, false))
	if got := lastStatus(t, body); got != "succeeded" {
		t.Fatalf("last status %q; body:\n%s", got, body)
	}
}

// assertOutputBeforeTerminal checks the output event precedes the final
// status, which is the last event: clients stop reading at a terminal status.
func assertOutputBeforeTerminal(t *testing.T, body string) {
	t.Helper()
	out := strings.Index(body, "event: output")
	last := strings.LastIndex(body, "event: status")
	if out < 0 || last < 0 || out > last {
		t.Fatalf("output must come before the terminal status:\n%s", body)
	}
	if strings.TrimSpace(body[last:]) == "" || strings.Contains(body[last+1:], "event: ") {
		t.Fatalf("terminal status is not the last event:\n%s", body)
	}
}

func TestOutputsPrecedeTerminalStatus(t *testing.T) {
	fastStream(t)
	// Terminal at the bridge's first snapshot: the late-client path.
	assertOutputBeforeTerminal(t, streamEvents(t, fakeComfy(t, 1, false, true)))
	// Terminal only at a later reconcile.
	assertOutputBeforeTerminal(t, streamEvents(t, fakeComfy(t, 3, true, true)))
}

// A job that finishes between the history read and the queue read is in
// neither. It must resolve to its terminal state, not unknown (which reads
// as expired).
func TestStatusOfSurvivesTheHistoryQueueRace(t *testing.T) {
	var historyCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /history/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{}`
		if historyCalls.Add(1) > 1 {
			body = `{"` + jobID + `":{"outputs":{},"status":{"status_str":"success","messages":[]}}}`
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Error(err)
		}
	})
	mux.HandleFunc("GET /queue", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`)); err != nil {
			t.Error(err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := &Proxy{comfy: comfy.New(srv.URL, ""), secret: []byte("s")}
	st, err := p.statusOf(context.Background(), jobID, "http://x")
	if err != nil || st.Status != "succeeded" {
		t.Fatalf("status = %q, %v; want succeeded", st.Status, err)
	}
	if historyCalls.Load() != 2 {
		t.Fatalf("history read %d times, want 2", historyCalls.Load())
	}
}
