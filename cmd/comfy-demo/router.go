package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// parseInterleaved lets flags sit before, between, or after the positional
// arguments. The flag package stops at the first positional one.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return rest, nil
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// workspaceBaseURL serves the Comfy Cloud job list.
const workspaceBaseURL = "https://platformapi.comfy.org"

// routerCommand drives client.Models. The key comes from COMFY_API_KEY, so
// it never appears on a command line or in shell history.
//
//	comfy-demo models                 list the catalog (free)
//	comfy-demo schema PROVIDER/MODEL  print a model's input schema (free)
//	comfy-demo run PROVIDER/MODEL '{"prompt":"..."}' [-out DIR]   billed
func routerCommand(name string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("comfy-demo "+name, flag.ContinueOnError)
	outDir := fs.String("out", ".", "directory a binary result is written into")
	limit := fs.Int("limit", 0, "catalog page size (models)")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if os.Getenv(sdk.APIKeyEnv) == "" {
		return fmt.Errorf("%s is not set; Comfy Router needs your Comfy API key (put it in .env and use the task targets)", sdk.APIKeyEnv)
	}
	client, err := sdk.New(sdk.WithClientInfo("comfy-demo"))
	if err != nil {
		return err
	}
	m := client.Models

	switch name {
	case "models":
		n := 0
		for cm, err := range m.List(ctx, sdk.ListModelsOptions{Limit: *limit}) {
			if err != nil {
				return err
			}
			n++
			note := ""
			if v, ok := cm.Billing["charges_on_policy_rejection"].(bool); ok && v {
				note = "  (charges on policy rejection)"
			}
			say(out, "%s%s", cm.ID, note)
		}
		say(out, "✓ %d model(s) on %s", n, m.BaseURL())
	case "schema":
		if len(rest) != 1 {
			return errors.New("usage: comfy-demo schema PROVIDER/MODEL")
		}
		res, err := m.Schema(ctx, rest[0], "")
		if err != nil {
			return err
		}
		say(out, "%s  etag=%s", rest[0], res.ETag)
		for _, line := range schemaSummary(res.Document) {
			say(out, "  %s", line)
		}
	case "run":
		if len(rest) != 2 {
			return errors.New(`usage: comfy-demo run PROVIDER/MODEL '{"prompt":"..."}' (billed)`)
		}
		var arguments map[string]any
		if err := json.Unmarshal([]byte(rest[1]), &arguments); err != nil {
			return fmt.Errorf("arguments must be a JSON object: %w", err)
		}
		say(out, "→ running %s on %s (billed)", rest[0], m.BaseURL())
		res, err := m.RunDetailed(ctx, rest[0], arguments, sdk.RunOptions{})
		if err != nil {
			return err
		}
		cost := "not reported"
		if res.CreditsUsed != nil {
			cost = *res.CreditsUsed
		}
		say(out, "  request %s, credits used: %s, replayed: %v", res.RequestID, cost, res.Replayed)
		if b := res.Output.Binary; b != nil {
			dest := filepath.Join(*outDir, "router-output"+extFor(b.ContentType))
			if err := os.WriteFile(dest, b.Content, 0o644); err != nil {
				return err
			}
			say(out, "  wrote %d bytes (%s) to %s", len(b.Content), b.ContentType, dest)
		} else {
			// Partners that answer JSON usually link the result with a
			// short-lived signed URL, so keep the whole answer on disk.
			pretty, err := json.MarshalIndent(res.Output.JSON, "", "  ")
			if err != nil {
				return err
			}
			dest := filepath.Join(*outDir, "router-output.json")
			if err := os.WriteFile(dest, append(pretty, '\n'), 0o644); err != nil {
				return err
			}
			say(out, "  wrote the JSON result to %s", dest)
			if status, ok := res.Output.JSON["status"].(string); ok {
				say(out, "  status: %s", status)
			}
		}
	}
	return nil
}

// jobsCommand lists recent jobs on a v2 deployment, newest first. Against
// Comfy Cloud it is a free check that the key works on the v2 API.
func jobsCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("comfy-demo jobs", flag.ContinueOnError)
	base := fs.String("base", "cloud", `base URL, or "cloud" for the Comfy Cloud workspace address`)
	max := fs.Int("n", 10, "how many jobs to show")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Comfy Cloud itself answers the job list with 501; the workspace
	// address serves it.
	if *base == "cloud" {
		*base = workspaceBaseURL
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client, err := sdk.New(sdk.WithBaseURL(*base), sdk.WithClientInfo("comfy-demo"))
	if err != nil {
		return err
	}
	n := 0
	for js, err := range client.ListJobs(ctx, sdk.ListOptions{Limit: *max}) {
		if err != nil {
			return err
		}
		when := "?"
		if js.CreateTime != nil {
			when = js.CreateTime.Format("2006-01-02 15:04")
		}
		say(out, "%s  %-10s %s", when, js.Status, js.ID)
		if n++; n >= *max {
			break
		}
	}
	say(out, "✓ %d job(s) from %s", n, client.BaseURL())
	return nil
}

// schemaSummary lists a model's run inputs: the JSON request body of the
// document's POST operation, with each property's type, default, and whether
// it is required, then the example request when the schema carries one.
func schemaSummary(doc map[string]any) []string {
	body := runBodySchema(doc)
	if body == nil {
		return []string{"(no JSON request body declared)"}
	}
	required := map[string]bool{}
	if req, ok := body["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	props, _ := body["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	var lines []string
	for _, n := range names {
		p, _ := resolveRef(doc, props[n]).(map[string]any)
		line := n
		if typ, ok := p["type"].(string); ok {
			line += " (" + typ + ")"
		} else if enum, ok := p["enum"].([]any); ok {
			line += fmt.Sprintf(" (one of %v)", enum)
		}
		if required[n] {
			line += " required"
		}
		if d, ok := p["default"]; ok {
			line += fmt.Sprintf(" default=%v", d)
		}
		lines = append(lines, line)
	}
	if ex, ok := body["example"]; ok {
		if raw, err := json.Marshal(ex); err == nil {
			lines = append(lines, "example: "+string(raw))
		}
	}
	return lines
}

// runBodySchema finds the application/json request body of the document's
// POST operation.
func runBodySchema(doc map[string]any) map[string]any {
	paths, _ := doc["paths"].(map[string]any)
	for _, ops := range paths {
		post, _ := ops.(map[string]any)["post"].(map[string]any)
		rb, _ := resolveRef(doc, post["requestBody"]).(map[string]any)
		content, _ := rb["content"].(map[string]any)
		media, _ := content["application/json"].(map[string]any)
		if schema, ok := resolveRef(doc, media["schema"]).(map[string]any); ok {
			return schema
		}
	}
	return nil
}

// resolveRef follows a local "#/..." $ref, once per level.
func resolveRef(doc map[string]any, v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	ref, ok := m["$ref"].(string)
	if !ok || !strings.HasPrefix(ref, "#/") {
		return v
	}
	var cur any = doc
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		node, ok := cur.(map[string]any)
		if !ok {
			return v
		}
		cur = node[part]
	}
	return cur
}

func extFor(contentType string) string {
	switch {
	case strings.HasPrefix(contentType, "image/png"):
		return ".png"
	case strings.HasPrefix(contentType, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(contentType, "image/webp"):
		return ".webp"
	case strings.HasPrefix(contentType, "video/mp4"):
		return ".mp4"
	case strings.HasPrefix(contentType, "audio/"):
		return ".audio"
	}
	return ".bin"
}
