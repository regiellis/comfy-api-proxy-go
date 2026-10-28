// Command comfy-api-proxy serves the Comfy API v2 in front of a self-hosted
// ComfyUI.
package main

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	"charm.land/fang/v2"
	"github.com/spf13/cobra"

	"github.com/regiellis/comfy-api-proxy-go/internal/config"
	"github.com/regiellis/comfy-api-proxy-go/internal/ui"
)

// version is set at build time with -ldflags "-X main.version=...". A
// binary from go install has no stamp, so its module version stands in.
var version = buildVersion(linkedVersion, readBuildInfo())

func readBuildInfo() *debug.BuildInfo {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info
	}
	return nil
}

// linkedVersion is what task build and GoReleaser stamp in.
var linkedVersion = "dev"

// buildVersion prefers the stamped version, then the module version Go
// records for go install (without its "v"), then "dev".
func buildVersion(linked string, info *debug.BuildInfo) string {
	if linked != "dev" || info == nil {
		return linked
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return strings.TrimPrefix(v, "v")
	}
	return linked
}

// Exit codes. 2 matches the reference for refused or invalid configuration.
const (
	exitOK      = 0
	exitRuntime = 1
	exitConfig  = 2
)

// errSilent carries an exit code for an outcome the command already
// reported, such as status finding nothing running.
type errSilent struct{ code int }

func (e *errSilent) Error() string { return "exit status " + strconv.Itoa(e.code) }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	root := newRootCmd(stdout)
	root.SetArgs(withDefaultCommand(root, args))
	root.SetOut(stdout)
	root.SetErr(stderr)

	errUI := ui.New(stderr)
	err := fang.Execute(context.Background(), root,
		fang.WithVersion(version),
		fang.WithErrorHandler(func(_ io.Writer, _ fang.Styles, err error) {
			if _, silent := errors.AsType[*errSilent](err); !silent {
				errUI.Error(err)
			}
		}),
	)
	if err == nil {
		return exitOK
	}
	if s, ok := errors.AsType[*errSilent](err); ok {
		return s.code
	}
	if _, ok := errors.AsType[*config.Error](err); ok {
		return exitConfig
	}
	return exitRuntime
}

// passthrough are first arguments that belong to the root command itself,
// including the ones fang and cobra add.
var passthrough = map[string]bool{
	"-h": true, "--help": true, "help": true, "-v": true, "--version": true,
	"completion": true, "man": true, "__complete": true, "__completeNoDesc": true,
}

// withDefaultCommand keeps the reference's bare-invocation form working:
// with no subcommand, or with flags first, the arguments go to "run".
func withDefaultCommand(root *cobra.Command, args []string) []string {
	if len(args) > 0 {
		if passthrough[args[0]] {
			return args
		}
		for _, c := range root.Commands() {
			if c.Name() == args[0] || c.HasAlias(args[0]) {
				return args
			}
		}
	}
	return append([]string{"run"}, args...)
}

func newRootCmd(stdout io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "comfy-api-proxy",
		Short: "Serve the Comfy API v2 in front of a self-hosted ComfyUI",
		Long: "Serve the Comfy API v2 in front of a self-hosted ComfyUI, so SDK code written " +
			"for Comfy Cloud runs unchanged against your own machine.\n\n" +
			"With no command, the arguments go to run.",
		Example: "  comfy-api-proxy\n" +
			"  comfy-api-proxy --comfyui http://127.0.0.1:8188 --port 8189\n" +
			"  comfy-api-proxy start --host global --token \"$TOKEN\"\n" +
			"  comfy-api-proxy status",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newRunCmd(stdout),
		newStartCmd(stdout),
		newStopCmd(stdout),
		newStatusCmd(stdout),
		newConfigCmd(stdout),
	)
	return root
}
