package tui

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

const (
	herdrGinoSource = "custom:gino"
	herdrGinoAgent  = "gino"
)

// herdrReporter optionally publishes Gino lifecycle state to Herdr.
// It is a no-op unless HERDR_ENV=1 and a pane ID plus herdr binary are available.
// Sequence numbers are seeded from Unix nanoseconds so a restarted Gino process
// is unlikely to replay IDs below Herdr's per-source watermark.
type herdrReporter struct {
	enabled bool
	bin     string
	paneID  string

	mu  sync.Mutex
	seq uint64
}

func newHerdrReporter() *herdrReporter {
	r := &herdrReporter{seq: uint64(time.Now().UnixNano())}

	if os.Getenv("HERDR_ENV") != "1" {
		return r
	}

	r.paneID = os.Getenv("HERDR_PANE_ID")
	if r.paneID == "" {
		return r
	}

	r.bin = os.Getenv("HERDR_BIN_PATH")
	if r.bin == "" {
		if bin, err := exec.LookPath("herdr"); err == nil {
			r.bin = bin
		}
	}
	if r.bin == "" {
		return r
	}

	r.enabled = true
	return r
}

func (r *herdrReporter) nextSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return r.seq
}

func (r *herdrReporter) run(args ...string) {
	if r == nil || !r.enabled {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Stdout = io.Discard
	if os.Getenv("GINO_DEBUG") != "" {
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stderr = io.Discard
	}
	if err := cmd.Run(); err != nil {
		if os.Getenv("GINO_DEBUG") != "" {
			log.Printf("herdr: %v", err)
		}
	}
}

func (r *herdrReporter) report(state, message string) {
	if r == nil || !r.enabled {
		return
	}

	args := []string{
		"pane", "report-agent", r.paneID,
		"--source", herdrGinoSource,
		"--agent", herdrGinoAgent,
		"--state", state,
		"--seq", strconv.FormatUint(r.nextSeq(), 10),
	}
	if message != "" {
		args = append(args, "--message", message)
	}

	r.run(args...)
}

func (r *herdrReporter) release() {
	if r == nil || !r.enabled {
		return
	}

	r.run(
		"pane", "release-agent", r.paneID,
		"--source", herdrGinoSource,
		"--agent", herdrGinoAgent,
		"--seq", strconv.FormatUint(r.nextSeq(), 10),
	)
}
