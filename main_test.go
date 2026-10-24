package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/instapods/wordpress-to-astro/wp2astro"
)

func TestParseArgsAcceptsFlagsAfterTheURL(t *testing.T) {
	o, err := parseArgs([]string{"https://example.com/blog/", "--out", "site", "-n", "5", "--dry-run"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.url != "https://example.com/blog/" || o.out != "site" || o.maxPosts != 5 || !o.dryRun {
		t.Errorf("parsed %+v", o)
	}
	if _, err := parseArgs([]string{"a.com", "b.com"}, io.Discard); err == nil {
		t.Error("two URLs must be refused")
	}
	if _, err := parseArgs(nil, io.Discard); err == nil {
		t.Error("no URL must be refused")
	}
}

func TestRunUsageExitCodes(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "give the address") {
		t.Errorf("no args: code %d, stderr %q", code, errb.String())
	}
	out.Reset()
	if code := run([]string{"--help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Usage:") {
		t.Errorf("--help: code %d, stdout %q", code, out.String())
	}
}

func TestCheckOutDirRefusesANonEmptyFolder(t *testing.T) {
	dir := t.TempDir()
	if err := checkOutDir(dir, false); err != nil {
		t.Errorf("empty dir: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "x"), nil, 0o644)
	if err := checkOutDir(dir, false); err == nil {
		t.Error("a non-empty dir must be refused without --force")
	}
	if err := checkOutDir(dir, true); err != nil {
		t.Errorf("--force: %v", err)
	}
}

func TestWriteProjectRefusesUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := writeProject(dir, []wp2astro.File{{Path: "../evil", Content: "x"}}); err == nil {
		t.Error("a path outside the output folder must be refused")
	}
	n, _, err := writeProject(dir, []wp2astro.File{{Path: "src/pages/[...path].astro", Content: "ok"}})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "src", "pages", "[...path].astro")); string(b) != "ok" {
		t.Errorf("content = %q", b)
	}
}
