// Command comfy-demo runs one workflow through the Go SDK against a Comfy API
// v2 deployment and downloads its outputs. It is the Go counterpart of the
// reference proxy's demo/run_demo.py.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// demoWorkflow is the reference demo's graph. Against a real ComfyUI pass
// -workflow with an API-format export instead.
var demoWorkflow = map[string]any{
	"3": map[string]any{"class_type": "KSampler", "inputs": map[string]any{"seed": 42, "steps": 1}},
	"9": map[string]any{"class_type": "SaveImage", "inputs": map[string]any{"images": []any{"3", 0}}},
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if _, werr := fmt.Fprintln(os.Stderr, "error:", err); werr != nil {
			os.Exit(1) // stderr is gone; the exit code still reports the failure
		}
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "models", "schema", "run":
			return routerCommand(args[0], args[1:], out)
		case "jobs":
			return jobsCommand(args[1:], out)
		}
	}
	fs := flag.NewFlagSet("comfy-demo", flag.ContinueOnError)
	base := fs.String("base", "http://127.0.0.1:8189", `base URL of the Comfy API v2 deployment, or "cloud" for Comfy Cloud`)
	key := fs.String("key", "", "API key, if the deployment requires one")
	workflow := fs.String("workflow", "", "API-format workflow JSON (default: the built-in demo graph)")
	outDir := fs.String("out", ".", "directory to download outputs into")
	events := fs.Bool("events", false, "follow the live event stream instead of polling")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *base == "cloud" {
		*base = sdk.CloudBaseURL
	}
	opts := []sdk.Option{sdk.WithBaseURL(*base), sdk.WithClientInfo("comfy-demo")}
	if *key != "" {
		opts = append(opts, sdk.WithAPIKey(*key))
	}
	client, err := sdk.New(opts...)
	if err != nil {
		return err
	}

	wf := sdk.NewWorkflow(demoWorkflow)
	if *workflow != "" {
		if wf, err = sdk.WorkflowFromFile(*workflow); err != nil {
			return err
		}
	}

	say(out, "→ running a workflow against %s", client.BaseURL())
	job, err := client.Submit(ctx, wf, sdk.SubmitOptions{})
	if err != nil {
		return err
	}
	say(out, "  submitted job %s", job.ID())

	if *events {
		for ev, err := range job.Events(ctx) {
			if err != nil {
				return err
			}
			switch e := ev.(type) {
			case *sdk.StatusChange:
				say(out, "  status %s", e.Status)
			case *sdk.Progress:
				say(out, "  progress %.0f%%", e.Value*100)
			case *sdk.Preview:
				say(out, "  preview %s (%d bytes)", e.ContentType, len(e.Data))
			case *sdk.OutputReady:
				say(out, "  output ready: %s", e.Output.Name())
			}
		}
	}
	if err := job.Result(ctx); err != nil {
		return err
	}
	outputs := job.Outputs()
	say(out, "  job %s → %s, %d output(s)", job.ID(), job.Status(), len(outputs))

	for i, o := range outputs {
		name := filepath.Base(o.Name())
		if len(outputs) > 1 {
			name = strconv.Itoa(i) + "_" + name
		}
		dest := filepath.Join(*outDir, name)
		if err := o.ToFile(ctx, dest, nil); err != nil {
			return err
		}
		info, err := os.Stat(dest)
		if err != nil {
			return err
		}
		say(out, "  downloaded %s '%s' → %s (%d bytes)", o.Type(), o.Name(), dest, info.Size())
	}
	say(out, "✓ done")
	return nil
}

func say(w io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(w, format+"\n", args...); err != nil {
		return // stdout closed; the demo's result is its exit code
	}
}
