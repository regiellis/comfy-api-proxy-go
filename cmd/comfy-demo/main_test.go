package main

import (
	"flag"
	"strings"
	"testing"
)

func TestParseInterleaved(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	out := fs.String("out", ".", "")
	limit := fs.Int("limit", 0, "")
	rest, err := parseInterleaved(fs, []string{"-limit", "3", "acme/x", `{"prompt":"p"}`, "-out", "/tmp/o"})
	if err != nil || strings.Join(rest, "|") != `acme/x|{"prompt":"p"}` || *out != "/tmp/o" || *limit != 3 {
		t.Fatalf("rest=%q out=%q limit=%d err=%v", rest, *out, *limit, err)
	}
}
