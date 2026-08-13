package tui

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func writeFakeHerdr(t *testing.T) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "herdr.log")
	bin = filepath.Join(dir, "herdr")
	script := `#!/bin/sh
exec 9>> "$HERDR_LOG"
flock 9
{
  printf '%s' "$1"
  shift
  for a in "$@"; do printf ' %s' "$a"; done
  printf '\n'
} >&9
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_LOG", logPath)
	return bin, logPath
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestHerdrReporterDisabledOutsideHerdr(t *testing.T) {
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	bin, logPath := writeFakeHerdr(t)
	t.Setenv("HERDR_BIN_PATH", bin)

	r := newHerdrReporter()
	r.report("idle", "ready")
	r.release()
	if r.enabled {
		t.Fatal("reporter should be disabled when HERDR_ENV is not 1")
	}
	if lines := readLines(t, logPath); len(lines) != 0 {
		t.Fatalf("expected no herdr commands, got %v", lines)
	}
}

func TestHerdrReporterLifecycleArgv(t *testing.T) {
	bin, logPath := writeFakeHerdr(t)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w3:p9")
	t.Setenv("HERDR_BIN_PATH", bin)

	r := newHerdrReporter()
	if !r.enabled {
		t.Fatal("expected reporter to enable under HERDR_ENV=1")
	}

	r.report("idle", "ready")
	r.report("working", "thinking")
	r.report("idle", "ready")
	r.release()

	lines := readLines(t, logPath)
	if len(lines) != 4 {
		t.Fatalf("got %d commands, want 4: %v", len(lines), lines)
	}

	wantPrefix := []string{
		"pane report-agent w3:p9 --source custom:gino --agent gino --state idle",
		"pane report-agent w3:p9 --source custom:gino --agent gino --state working",
		"pane report-agent w3:p9 --source custom:gino --agent gino --state idle",
		"pane release-agent w3:p9 --source custom:gino --agent gino",
	}
	var seqs []uint64
	for i, line := range lines {
		if !strings.Contains(line, wantPrefix[i]) {
			t.Fatalf("command %d = %q, want prefix %q", i, line, wantPrefix[i])
		}
		fields := strings.Fields(line)
		for j, f := range fields {
			if f == "--seq" && j+1 < len(fields) {
				n, err := strconv.ParseUint(fields[j+1], 10, 64)
				if err != nil {
					t.Fatalf("seq: %v", err)
				}
				seqs = append(seqs, n)
			}
		}
	}
	if len(seqs) != 4 {
		t.Fatalf("expected 4 sequence numbers, got %v from %v", seqs, lines)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("sequence numbers must increase: %v", seqs)
		}
	}
}

func TestHerdrReporterBrokenBinaryDoesNotPanic(t *testing.T) {
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_BIN_PATH", filepath.Join(t.TempDir(), "missing-herdr"))

	r := newHerdrReporter()
	r.report("working", "thinking")
	r.release()
}

func TestHerdrReporterConcurrentSequence(t *testing.T) {
	bin, logPath := writeFakeHerdr(t)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_BIN_PATH", bin)

	r := newHerdrReporter()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.report("working", "thinking")
		}()
	}
	wg.Wait()

	lines := readLines(t, logPath)
	if len(lines) != 8 {
		t.Fatalf("got %d commands, want 8", len(lines))
	}
	seen := map[uint64]bool{}
	for _, line := range lines {
		fields := strings.Fields(line)
		for j, f := range fields {
			if f == "--seq" && j+1 < len(fields) {
				n, err := strconv.ParseUint(fields[j+1], 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				if seen[n] {
					t.Fatalf("duplicate seq %d", n)
				}
				seen[n] = true
			}
		}
	}
	if len(seen) != 8 {
		t.Fatalf("expected 8 distinct seqs, got %v", seen)
	}
}
