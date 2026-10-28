package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/regiellis/comfy-api-proxy-go/internal/comfy"
	"github.com/regiellis/comfy-api-proxy-go/internal/httpx"
)

func notFoundJob(w http.ResponseWriter, id string) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "No job "+id+".", nil)
}

func upstreamError(w http.ResponseWriter, message string) {
	httpx.WriteError(w, http.StatusServiceUnavailable, "upstream_unreachable", message, nil)
}

// internalError reports a failure of the proxy's own state, such as a
// --state-dir write. It never echoes the error text to the client.
func (p *Proxy) internalError(w http.ResponseWriter, err error) {
	p.logf("error: %v", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Internal proxy error.", nil)
}

// stateError routes a statusOf failure: unreachable upstream is a 503,
// anything else is ours.
func (p *Proxy) stateError(w http.ResponseWriter, err error) {
	if errors.Is(err, comfy.ErrUnreachable) {
		upstreamError(w, "Could not reach ComfyUI.")
		return
	}
	p.internalError(w, err)
}

type submitExtras struct {
	metadata *string
	priority *int64
}

// parseSubmitExtras validates the optional metadata and priority fields.
func parseSubmitExtras(body map[string]any) (submitExtras, string) {
	var ex submitExtras
	if raw, ok := body["metadata"]; ok && raw != nil {
		s, isStr := raw.(string)
		if !isStr {
			return ex, "'metadata' must be a string (opaque label)."
		}
		if len(s) > maxMetadataBytes {
			return ex, fmt.Sprintf("'metadata' exceeds %d UTF-8 bytes.", maxMetadataBytes)
		}
		ex.metadata = &s
	}
	if raw, ok := body["priority"]; ok && raw != nil {
		num, isNum := raw.(json.Number)
		if !isNum {
			return ex, "'priority' must be an integer (advisory)."
		}
		n, err := strconv.ParseInt(num.String(), 10, 64)
		if err != nil {
			if strings.ContainsAny(num.String(), ".eE") {
				return ex, "'priority' must be an integer (advisory)."
			}
			return ex, fmt.Sprintf("'priority' must be between %d and %d.", minPriority, maxPriority)
		}
		if n < minPriority || n > maxPriority {
			return ex, fmt.Sprintf("'priority' must be between %d and %d.", minPriority, maxPriority)
		}
		ex.priority = &n
	}
	return ex, ""
}

// validExtraData checks the closed extra_data object. The only key the v2
// contract allows is api_key_comfy_org, a string, which rides the /prompt
// body to authenticate partner nodes and is never stored or logged.
func validExtraData(raw any) (map[string]any, string) {
	if raw == nil {
		return nil, ""
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, "'extra_data' accepts only the 'api_key_comfy_org' field."
	}
	for k := range m {
		if k != "api_key_comfy_org" {
			return nil, "'extra_data' accepts only the 'api_key_comfy_org' field."
		}
	}
	if v, present := m["api_key_comfy_org"]; present {
		if _, isStr := v.(string); !isStr {
			return nil, "'extra_data.api_key_comfy_org' must be a string."
		}
	}
	return m, ""
}

func (p *Proxy) submit(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeJSON(http.MaxBytesReader(w, r.Body, p.maxUpload+1<<20), &body); err != nil || body == nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "Body must be JSON.", nil)
		return
	}
	if body["webhook_url"] != nil || body["inputs"] != nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_workflow",
			"'webhook_url' and 'inputs' are reserved and not accepted.", nil)
		return
	}
	workflow, ok := body["workflow"].(map[string]any)
	if !ok {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_workflow", "Missing 'workflow' (API-format graph).", nil)
		return
	}
	if isUIFormat(workflow) {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "workflow_format_ui",
			"This is a UI-export graph. Export the workflow in API format instead.", nil)
		return
	}
	extras, msg := parseSubmitExtras(body)
	if msg != "" {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_request", msg, nil)
		return
	}
	extraData, msg := validExtraData(body["extra_data"])
	if msg != "" {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request", msg, nil)
		return
	}

	var missing []string
	resolved := p.rewriteAssetRefs(workflow, &missing)
	if len(missing) > 0 {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "missing_asset",
			"Unresolvable core/ASSET reference(s): "+strings.Join(missing, ", "),
			map[string]any{"missing_ids": missing})
		return
	}

	// ComfyUI requires prompt_id to be a canonical UUID. The job id doubles
	// as prompt_id and as client_id: ComfyUI addresses a prompt's WebSocket
	// events at the client that submitted it, so a per-job client_id gives
	// each job's SSE bridge its own addressable target.
	jobID, err := newUUID()
	if err != nil {
		p.internalError(w, err)
		return
	}

	// Idempotency-Key is single-use and rejected on reuse, with no replay.
	// The claim is released only when ComfyUI definitely created no job. On
	// an unknown outcome the key stays held, so a retry cannot double-submit.
	key, hasKey := "", false
	if vals := r.Header.Values("Idempotency-Key"); len(vals) > 0 {
		key, hasKey = vals[0], true
		if key == "" || utf8.RuneCountInString(key) > maxIdempotencyKeyLen {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("Idempotency-Key must be 1-%d characters.", maxIdempotencyKeyLen), nil)
			return
		}
		fresh, err := p.idem.claim(key)
		if err != nil {
			p.internalError(w, err)
			return
		}
		if !fresh {
			httpx.WriteError(w, http.StatusUnprocessableEntity, "idempotency_key_reuse",
				"This Idempotency-Key has already been used. Keys are single-use; "+
					"poll or list your jobs instead of resubmitting with the same key.", nil)
			return
		}
	}

	payload := map[string]any{"prompt": resolved, "prompt_id": jobID, "client_id": jobID}
	if len(extraData) > 0 {
		payload["extra_data"] = extraData
	}
	resp, err := p.comfy.Do(r.Context(), http.MethodPost, "/prompt", payload)
	if err != nil {
		upstreamError(w, "Could not reach ComfyUI to submit the job.")
		return
	}
	if resp.Status != http.StatusOK {
		if hasKey {
			if err := p.idem.release(key); err != nil {
				p.internalError(w, err)
				return
			}
		}
		var rejected struct {
			Error *struct {
				Message *string `json:"message"`
			} `json:"error"`
			NodeErrors map[string]any `json:"node_errors"`
		}
		if resp.IsJSON() {
			if err := decodeJSON(strings.NewReader(string(resp.Body)), &rejected); err != nil {
				rejected.Error, rejected.NodeErrors = nil, nil
			}
		}
		message := "Workflow rejected."
		if rejected.Error != nil && rejected.Error.Message != nil {
			message = *rejected.Error.Message
		}
		nodeErrors := rejected.NodeErrors
		if nodeErrors == nil {
			nodeErrors = map[string]any{}
		}
		httpx.WriteError(w, http.StatusUnprocessableEntity, "invalid_workflow", message,
			map[string]any{"node_errors": nodeErrors})
		return
	}

	// Store the resolved graph, which is what actually ran.
	stored, err := encodeJSON(resolved)
	if err != nil {
		p.internalError(w, err)
		return
	}
	rec := JobRecord{ID: jobID, CreatedAt: now(), ClientID: jobID, Metadata: extras.metadata, Priority: extras.priority, Workflow: stored}
	if err := p.jobs.remember(rec); err != nil {
		p.internalError(w, err)
		return
	}
	st := jobState{Status: "queued", Outputs: []output{}}
	httpx.WriteJSON(w, http.StatusCreated, jobJSON(jobID, st, rec, true, p.base(r)))
}

var listableStatuses = map[string]bool{
	"queued": true, "running": true, "succeeded": true, "canceling": true,
	"canceled": true, "failed": true, "expired": true,
}

func (p *Proxy) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := defaultListJobs
	if raw, ok := q["limit"]; ok {
		n, err := strconv.Atoi(strings.TrimSpace(raw[0]))
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request", "'limit' must be an integer.", nil)
			return
		}
		limit = n
	}
	if limit < 1 || limit > maxListJobs {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("'limit' must be between 1 and %d.", maxListJobs), nil)
		return
	}
	var filter map[string]bool
	if raw := q.Get("status"); raw != "" {
		filter = map[string]bool{}
		var bad []string
		for s := range strings.SplitSeq(raw, ",") {
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			filter[s] = true
			if !listableStatuses[s] {
				bad = append(bad, s)
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request",
				"Unknown status filter value(s): "+strings.Join(dedupe(bad), ", ")+".", nil)
			return
		}
	}

	base := p.base(r)
	ids := p.jobs.newestFirst()
	jobs := []jobBody{}
	scanned := 0
	for _, id := range ids {
		if len(jobs) >= limit || scanned >= maxJobScan {
			break
		}
		scanned++
		st, err := p.statusOf(r.Context(), id, base)
		if err != nil {
			p.stateError(w, err)
			return
		}
		rec, known, err := p.jobs.meta(id)
		if err != nil {
			p.internalError(w, err)
			return
		}
		job := jobJSON(id, st, rec, known, base)
		if filter != nil && !filter[job.Status] {
			continue
		}
		jobs = append(jobs, job)
	}
	// truncated tells "no more matches" apart from "stopped looking".
	truncated := len(jobs) < limit && scanned < len(ids)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "truncated": truncated})
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func (p *Proxy) getJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		notFoundJob(w, id)
		return
	}
	base := p.base(r)
	st, err := p.statusOf(r.Context(), id, base)
	if err != nil {
		p.stateError(w, err)
		return
	}
	rec, known, err := p.jobs.meta(id)
	if err != nil {
		p.internalError(w, err)
		return
	}
	if st.Status == "unknown" && !known {
		notFoundJob(w, id)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, jobJSON(id, st, rec, known, base))
}

// getJobWorkflow returns the proxy's own record of the resolved graph, never
// ComfyUI's /history copy: that one carries the partner API key in its
// extra_data, and reading it back would risk leaking the credential.
func (p *Proxy) getJobWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		notFoundJob(w, id)
		return
	}
	wf, err := p.jobs.workflow(id)
	if err != nil {
		p.internalError(w, err)
		return
	}
	if wf == nil || !isJSONObject(wf) {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "No recoverable workflow for job "+id+".", nil)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"workflow": wf, "format": "api"})
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "{")
}

func (p *Proxy) cancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		notFoundJob(w, id)
		return
	}
	base := p.base(r)
	_, known, err := p.jobs.meta(id)
	if err != nil {
		p.internalError(w, err)
		return
	}
	if !known {
		// A proxy restart forgets records, but ComfyUI may still hold the
		// job, so only a job unknown to both is a 404.
		st, err := p.statusOf(r.Context(), id, base)
		if err != nil {
			p.stateError(w, err)
			return
		}
		if st.Status == "unknown" {
			notFoundJob(w, id)
			return
		}
	}
	if _, err := p.comfy.Do(r.Context(), http.MethodPost, "/api/jobs/"+id+"/cancel", nil); err != nil {
		upstreamError(w, "Could not reach ComfyUI to cancel.")
		return
	}
	st, err := p.statusOf(r.Context(), id, base)
	if err != nil {
		p.stateError(w, err)
		return
	}
	// A running job reports canceling until the interrupt lands at the next
	// node boundary.
	if st.Status == "running" {
		st.Status = "canceling"
	}
	rec, known, err := p.jobs.meta(id)
	if err != nil {
		p.internalError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, jobJSON(id, st, rec, known, base))
}
