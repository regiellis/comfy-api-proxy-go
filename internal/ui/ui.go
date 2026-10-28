// Package ui renders everything the CLI prints. On a terminal it is styled;
// piped, redirected, or with NO_COLOR it degrades to plain text that greps
// and logs cleanly, because the start command's child writes to a log file.
//
// One truecolor accent marks identity (the title, the panel edge) and
// degrades with the terminal. Status rides the named ANSI colors, so the
// user's theme decides how red and green look, and every colored state also
// carries a symbol or a number, so nothing is lost when color is.
// Secondary text uses bright black, which themes define against their own
// background, rather than the dim attribute.
package ui

import (
	"fmt"
	"image/color"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

var (
	accent color.Color = lipgloss.Color("#C2703D")
	green  color.Color = lipgloss.Green
	yellow color.Color = lipgloss.Yellow
	red    color.Color = lipgloss.Red
	dim    color.Color = lipgloss.BrightBlack
)

const keyWidth = 10

// UI writes to one stream.
type UI struct {
	w      io.Writer
	styled bool
}

// New wraps w. Styling turns on only when w is a terminal that takes color.
func New(w io.Writer) *UI {
	env := os.Environ()
	return &UI{w: colorprofile.NewWriter(w, env), styled: colorprofile.Detect(w, env) > colorprofile.ASCII}
}

func (u *UI) println(s string) {
	if _, err := fmt.Fprintln(u.w, s); err != nil {
		return // the stream is gone; there is no one left to tell
	}
}

func fg(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

func bold(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Bold(true).Foreground(c) }

// Field is one labelled line in a block.
type Field struct {
	Key, Value, Note string
}

func (f Field) plain() string {
	if f.Note == "" {
		return f.Key + ": " + f.Value
	}
	return f.Key + ": " + f.Value + " (" + f.Note + ")"
}

func (f Field) styled() string {
	line := fg(dim).Render(pad(f.Key, keyWidth)) + f.Value
	if f.Note != "" {
		line += "  " + fg(dim).Render(f.Note)
	}
	return line
}

func pad(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s + " "
}

// Banner is the startup summary for the run command.
type Banner struct {
	Version string
	Fields  []Field
	URLs    []string
}

// Startup prints the banner: a rounded panel on a terminal, key: value lines
// otherwise.
func (u *UI) Startup(b Banner) {
	if !u.styled {
		u.println("comfy-api-proxy " + b.Version + " listening")
		for _, f := range b.Fields {
			u.println("  " + f.plain())
		}
		for _, url := range b.URLs {
			u.println("  url: " + url)
		}
		return
	}
	lines := []string{bold(accent).Render("comfy-api-proxy") + "  " + fg(dim).Render(b.Version), ""}
	for _, f := range b.Fields {
		lines = append(lines, f.styled())
	}
	lines = append(lines, "")
	for i, url := range b.URLs {
		label := "local"
		if i > 0 {
			label = "network"
		}
		lines = append(lines, bold(green).Render("➜")+" "+fg(dim).Render(pad(label, keyWidth-2))+lipgloss.NewStyle().Bold(true).Render(url))
	}
	u.println(lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accent).
		Padding(0, 2).
		Render(strings.Join(lines, "\n")))
	u.println(fg(dim).Render("  Ctrl+C to stop"))
}

// Fields prints labelled lines, for status and config output.
func (u *UI) Fields(fields []Field) {
	for _, f := range fields {
		if u.styled {
			u.println("  " + f.styled())
		} else {
			u.println(f.plain())
		}
	}
}

// Success prints a completed action.
func (u *UI) Success(msg string) { u.mark("✓", green, msg) }

// Info prints a neutral state line.
func (u *UI) Info(msg string) { u.mark("•", dim, msg) }

// Warn prints something the user should look at.
func (u *UI) Warn(msg string) { u.mark("!", yellow, msg) }

func (u *UI) mark(sym string, c color.Color, msg string) {
	if !u.styled {
		u.println(msg)
		return
	}
	u.println(bold(c).Render(sym) + " " + msg)
}

// Error prints a failure. The first line is the headline; further lines are
// the fix, shown dimmed beneath it.
func (u *UI) Error(err error) {
	msg := err.Error()
	if !u.styled {
		u.println("error: " + msg)
		return
	}
	head, rest, _ := strings.Cut(msg, "\n")
	u.println(bold(red).Render("✗") + " " + lipgloss.NewStyle().Bold(true).Render(head))
	for line := range strings.SplitSeq(rest, "\n") {
		if line != "" {
			u.println("  " + fg(dim).Render(line))
		}
	}
}

// Request prints one access-log line.
func (u *UI) Request(at time.Time, method, path string, status int, took time.Duration) {
	dur := took.Round(10 * time.Microsecond).String()
	if !u.styled {
		u.println(at.Format(time.RFC3339) + " " + method + " " + path + " " + strconv.Itoa(status) + " " + dur)
		return
	}
	c := green
	switch {
	case status >= 500:
		c = red
	case status >= 400:
		c = yellow
	case status >= 300:
		c = dim
	}
	u.println(fg(dim).Render(at.Format("15:04:05")) + "  " +
		bold(c).Render(strconv.Itoa(status)) + "  " +
		lipgloss.NewStyle().Width(7).Render(method) + path + "  " + fg(dim).Render(dur))
}
