package sdk

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func TestSetInputCreatesMissingNode(t *testing.T) {
	wf := NewWorkflow(map[string]any{})
	wf.SetInput("7", "seed", 42)
	if !reflect.DeepEqual(wf.Graph["7"], map[string]any{"inputs": map[string]any{"seed": 42}}) {
		t.Fatalf("graph = %v", wf.Graph)
	}
}

func TestWorkflowFromJSONKeepsExactNumbers(t *testing.T) {
	wf, err := WorkflowFromJSON([]byte(`{"3":{"inputs":{"seed":18446744073709551615}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := wf.Graph["3"].(map[string]any)["inputs"].(map[string]any)["seed"]; got.(interface{ String() string }).String() != "18446744073709551615" {
		t.Fatalf("seed = %v", got)
	}
	if _, err := WorkflowFromJSON([]byte(`[1]`)); err == nil {
		t.Fatal("a non-object workflow parsed")
	}
}

func inputs(wf *Workflow, node string) map[string]any {
	return wf.Graph[node].(map[string]any)["inputs"].(map[string]any)
}

func TestRemoveNodeRewiresPassThrough(t *testing.T) {
	wf, err := WorkflowFromJSON([]byte(`{
		"1": {"inputs": {"unet_name": "z.safetensors"}, "class_type": "UNETLoader"},
		"2": {"inputs": {"lora_name": "e.safetensors", "strength_model": 1, "model": ["1", 0]}, "class_type": "LoraLoaderModelOnly"},
		"3": {"inputs": {"attention": "pytorch attention", "model": ["2", 0]}, "class_type": "ModelAttentionBackend"},
		"4": {"inputs": {"seed": 0, "model": ["3", 0]}, "class_type": "KSampler"}}`))
	if err != nil {
		t.Fatal(err)
	}
	wf.RemoveNode("3")
	if _, ok := wf.Graph["3"]; ok {
		t.Fatal("node not removed")
	}
	if !reflect.DeepEqual(inputs(wf, "4")["model"], []any{"2", 0}) {
		t.Fatalf("model = %v", inputs(wf, "4")["model"])
	}
}

func TestRemoveNodeRedirectsEveryConsumer(t *testing.T) {
	wf := NewWorkflow(map[string]any{
		"1": map[string]any{"inputs": map[string]any{"image": "example.png"}, "class_type": "LoadImage"},
		"2": map[string]any{"inputs": map[string]any{"images": []any{"5", 0}}, "class_type": "PreviewImage"},
		"3": map[string]any{"inputs": map[string]any{"images": []any{"5", 0}}, "class_type": "PreviewImage"},
		"5": map[string]any{"inputs": map[string]any{"megapixels": 1, "image": []any{"1", 0}}, "class_type": "ImageScaleToTotalPixels"},
	})
	wf.RemoveNode("5")
	for _, n := range []string{"2", "3"} {
		if !reflect.DeepEqual(inputs(wf, n)["images"], []any{"1", 0}) {
			t.Fatalf("node %s images = %v", n, inputs(wf, n)["images"])
		}
	}
}

func TestRemoveNodeWithSeveralLinksDropsReferences(t *testing.T) {
	wf := NewWorkflow(map[string]any{
		"1": map[string]any{"inputs": map[string]any{}},
		"2": map[string]any{"inputs": map[string]any{}},
		"3": map[string]any{"inputs": map[string]any{"a": []any{"1", 0}, "b": []any{"2", 0}}},
		"4": map[string]any{"inputs": map[string]any{"x": []any{"3", 0}, "keep": 5}},
	})
	wf.RemoveNode("3")
	if _, ok := inputs(wf, "4")["x"]; ok || inputs(wf, "4")["keep"] != 5 {
		t.Fatalf("inputs = %v", inputs(wf, "4"))
	}
	wf.RemoveNode("nope") // no-op
}

func TestSSEDecoder(t *testing.T) {
	var d sseDecoder
	var got []rawEvent
	for _, line := range []string{
		": keepalive", "event: progress", "data: {\"value\": 0.5}", "",
		"data: 7", "",
		"event: log", "data: not json", "",
		"event: empty", "",
		"data: {\"a\":", "data: 1}", "",
	} {
		if ev, ok := d.push(line); ok {
			got = append(got, ev)
		}
	}
	want := []rawEvent{
		{"progress", map[string]any{"value": 0.5}},
		{"message", map[string]any{"value": float64(7)}},
		{"log", map[string]any{"raw": "not json"}},
		{"message", map[string]any{"a": float64(1)}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func frames(fs ...string) string {
	out := ""
	for _, f := range fs {
		out += f + "\n\n"
	}
	return out
}

func TestEventsStreamToTerminal(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.events = []string{frames(
		`event: status`+"\n"+`data: {"status":"running"}`,
		`event: progress`+"\n"+`data: {"value":0.5,"nodes_done":1,"nodes_total":2,"current_node":"3","step":1,"steps":2}`,
		`event: preview`+"\n"+`data: {"node_id":"3","content_type":"image/png","data_base64":"iVBORw=="}`,
		`event: output`+"\n"+`data: {"node_id":"9","name":"out.png","type":"image","content_type":"image/png","size_bytes":4,"id":"o9","hash":null,"url":"http://x/c","url_expires_at":"2026-10-18T10:00:00Z","job_id":"job-1"}`,
		`event: status`+"\n"+`data: {"status":"succeeded"}`,
		`event: status`+"\n"+`data: {"status":"never-read"}`,
	)}
	c := s.client(t)
	job, err := c.Submit(context.Background(), simpleWorkflow(), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for ev, err := range job.Events(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		switch e := ev.(type) {
		case *StatusChange:
			kinds = append(kinds, "status:"+e.Status)
		case *Progress:
			if e.Value != 0.5 || *e.CurrentNode != "3" || *e.Steps != 2 {
				t.Fatalf("progress = %+v", e)
			}
			kinds = append(kinds, "progress")
		case *Preview:
			if e.NodeID != "3" || len(e.Data) == 0 {
				t.Fatalf("preview = %+v", e)
			}
			kinds = append(kinds, "preview")
		case *OutputReady:
			if e.Output.ID() != "o9" {
				t.Fatalf("output = %v", e.Output)
			}
			kinds = append(kinds, "output")
		}
	}
	want := []string{"status:running", "progress", "preview", "output", "status:succeeded"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("events = %v", kinds)
	}
}

func TestEventsFallBackToPollingWhenStreamEndsEarly(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.events = []string{frames(`event: status` + "\n" + `data: {"status":"running"}`)}
	s.jobStatus = []string{StatusRunning, StatusSucceeded}
	job, err := s.client(t).Submit(context.Background(), simpleWorkflow(), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var last *StatusChange
	for ev, err := range job.Events(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		if sc, ok := ev.(*StatusChange); ok {
			last = sc
		}
	}
	if last == nil || last.Status != StatusSucceeded {
		t.Fatalf("last status = %+v", last)
	}
	if s.eventsConns < 2 {
		t.Fatalf("stream was not reconnected: %d connections", s.eventsConns)
	}
}

func TestEventsEndQuietlyOn501AndSurfaceOtherErrors(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	c := s.client(t)
	job, err := c.Submit(context.Background(), simpleWorkflow(), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.routes["GET /api/v2/jobs/job-1/events"] = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 501, envelope("not_implemented", "no stream"))
	}
	for _, err := range job.Events(context.Background()) {
		t.Fatalf("a 501 produced %v", err)
	}
	s.routes["GET /api/v2/jobs/job-1/events"] = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 404, envelope("not_found", "gone"))
	}
	var got error
	for _, err := range job.Events(context.Background()) {
		got = err
	}
	if !errors.Is(got, ErrNotFound) {
		t.Fatalf("got %v", got)
	}
}

func TestEventsStopWhenConsumerBreaks(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.events = []string{frames(`event: status`+"\n"+`data: {"status":"running"}`, `event: status`+"\n"+`data: {"status":"running"}`)}
	job, err := s.client(t).Submit(context.Background(), simpleWorkflow(), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, err := range job.Events(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		break
	}
	if n != 1 {
		t.Fatalf("got %d events after break", n)
	}
}
