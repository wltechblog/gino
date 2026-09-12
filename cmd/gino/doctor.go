package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

// doctorCheck is one line of the report.
type doctorCheck struct {
	name   string
	status string // "ok", "warn", "fail", "skip"
	detail string
}

const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
	statusSkip = "skip"
	gibBytes   = uint64(1024 * 1024 * 1024)
)

func statusIcon(s string) string {
	switch s {
	case statusOK:
		return "✓"
	case statusWarn:
		return "!"
	case statusFail:
		return "✗"
	default:
		return "-"
	}
}

// redactedKey shows enough of a key to identify it without leaking it.
func redactedKey(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "…" + s[len(s)-4:]
}

// readMemTotal parses /proc/meminfo MemTotal (bytes). Returns error on
// non-Linux or missing file.
func readMemTotal() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				var kb uint64
				if _, err := fmt.Sscanf(fields[1], "%d", &kb); err == nil {
					return kb * 1024, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("MemTotal not found")
}

// checkSystem inspects host basics: version/arch, memory, systemd.
func checkSystem(checks *[]doctorCheck) {
	add := func(name, status, detail string) {
		*checks = append(*checks, doctorCheck{name, status, detail})
	}

	add("binary", statusOK, fmt.Sprintf("gino v%s %s/%s", version, runtime.GOOS, runtime.GOARCH))

	if m, err := readMemTotal(); err == nil {
		gb := float64(m) / float64(gibBytes)
		switch {
		case m >= 2*gibBytes:
			add("memory", statusOK, fmt.Sprintf("%.1f GB", gb))
		case m >= gibBytes:
			add("memory", statusWarn, fmt.Sprintf("%.1f GB — advanced brain (Ollama) may be sluggish; basic brain works fine", gb))
		default:
			add("memory", statusWarn, fmt.Sprintf("%.1f GB — advanced brain (Ollama) probably won't work suitably; basic brain works fine", gb))
		}
	} else {
		add("memory", statusSkip, "unknown (/proc/meminfo unavailable)")
	}

	if _, err := exec.LookPath("systemctl"); err == nil {
		add("systemd", statusOK, "systemctl present")
	} else {
		add("systemd", statusSkip, "not found (gateway service requires systemd; TUI mode works without)")
	}
}

// expandTilde expands a leading ~/ the same way the agent does.
func expandTilde(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// checkWorkspace verifies home dir, config, workspace, skills, git identity.
func checkWorkspace(checks *[]doctorCheck, homeDir string, cfg config.Config) {
	add := func(name, status, detail string) {
		*checks = append(*checks, doctorCheck{name, status, detail})
	}

	if _, err := os.Stat(filepath.Join(homeDir, "config.json")); err == nil {
		add("config", statusOK, homeDir)
	} else {
		add("config", statusFail, fmt.Sprintf("no config.json under %s — run `gino onboard`", homeDir))
	}

	ws := cfg.Agents.Defaults.Workspace
	switch {
	case ws == "":
		add("workspace", statusSkip, "not set (defaults to profile workspace)")
	case statIsDir(expandTilde(ws)):
		add("workspace", statusOK, ws)
	default:
		add("workspace", statusFail, fmt.Sprintf("%s does not exist or is not a directory", ws))
	}

	if _, err := os.Stat(filepath.Join(homeDir, "skills")); err == nil {
		add("skills", statusOK, "skills directory present")
	} else {
		add("skills", statusSkip, "no skills directory yet (optional)")
	}

	if out, err := exec.Command("git", "config", "--global", "user.email").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		add("git identity", statusOK, strings.TrimSpace(string(out)))
	} else if _, err := exec.LookPath("git"); err != nil {
		add("git identity", statusWarn, "git not installed — the agent cannot commit")
	} else {
		add("git identity", statusWarn, "git user.email not set — the agent cannot commit as you")
	}
}

func statIsDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// checkProvider probes the configured LLM endpoint. It only reports
// reachability and auth shape — never performs a chat completion (no spend).
func checkProvider(checks *[]doctorCheck, cfg config.Config) {
	add := func(name, status, detail string) {
		*checks = append(*checks, doctorCheck{name, status, detail})
	}

	oc := cfg.Providers.OpenAI
	if oc == nil {
		add("provider", statusFail, "no providers.openai block — the agent has no LLM to talk to")
		return
	}

	base := strings.TrimRight(oc.APIBase, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	key := oc.APIKey

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/models", nil)
	if err != nil {
		add("provider", statusFail, "malformed API base URL: "+err.Error())
		return
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		add("provider", statusFail, fmt.Sprintf("%s unreachable: %v", base, err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case 200:
		add("provider", statusOK, fmt.Sprintf("%s reachable (key %s)", base, redactedKey(key)))
	case 401, 403:
		add("provider", statusFail, fmt.Sprintf("%s rejected credentials (HTTP %d) — check apiKey", base, resp.StatusCode))
	case 404:
		// some OpenAI-compatible endpoints don't implement /models
		add("provider", statusWarn, fmt.Sprintf("%s reachable but /models returned 404 — chat may still work", base))
	default:
		add("provider", statusWarn, fmt.Sprintf("%s responded HTTP %d — unusual status for /models", base, resp.StatusCode))
	}
}

// checkBrain reports the embedding tier actually in effect.
func checkBrain(checks *[]doctorCheck, cfg config.Config) {
	add := func(name, status, detail string) {
		*checks = append(*checks, doctorCheck{name, status, detail})
	}

	if cfg.Brain == nil || !cfg.Brain.Enabled {
		add("brain", statusSkip, "disabled — basic keyword memory only (fine)")
		return
	}

	ollamaURL := cfg.Brain.OllamaURL
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(ollamaURL + "/api/version")
	if err == nil && resp.StatusCode == 200 {
		ver := ""
		if data, readErr := io.ReadAll(resp.Body); readErr == nil {
			var m map[string]any
			if json.Unmarshal(data, &m) == nil {
				if v, ok := m["version"].(string); ok {
					ver = " v" + v
				}
			}
		}
		_ = resp.Body.Close()
		add("brain", statusOK, fmt.Sprintf("Ollama%s at %s (advanced brain)", ver, ollamaURL))
		return
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	if cfg.Brain.RemoteAPIBase != "" && cfg.Brain.RemoteAPIKey != "" {
		add("brain", statusWarn, fmt.Sprintf("Ollama not running at %s — remote embedding API will be used", ollamaURL))
		return
	}
	add("brain", statusWarn, fmt.Sprintf("Ollama not running at %s — keyword-only mode (search works, no semantic ranking)", ollamaURL))
}

// checkService reports gateway systemd state when Telegram mode is configured.
func checkService(checks *[]doctorCheck, cfg config.Config) {
	add := func(name, status, detail string) {
		*checks = append(*checks, doctorCheck{name, status, detail})
	}

	if !cfg.Channels.Telegram.Enabled {
		add("gateway service", statusSkip, "Telegram not configured — use `gino chat` for the TUI")
		return
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		add("gateway service", statusSkip, "no systemd on this host")
		return
	}
	if _, err := os.Stat("/etc/systemd/system/gino-gateway.service"); err == nil {
		if out, err := exec.Command("systemctl", "is-active", "gino-gateway").Output(); err == nil {
			state := strings.TrimSpace(string(out))
			if state == "active" {
				add("gateway service", statusOK, "running — logs: journalctl -u gino-gateway -f")
				return
			}
			add("gateway service", statusWarn, fmt.Sprintf("unit installed but %s — start with: systemctl start gino-gateway", state))
			return
		}
		add("gateway service", statusOK, "unit installed")
		return
	}
	add("gateway service", statusWarn, "Telegram enabled but no systemd unit — re-run install.sh (or create the unit) to run the gateway on boot")
}

// checkReasoning reports the reasoning-effort vocabulary in effect.
func checkReasoning(checks *[]doctorCheck, cfg config.Config) {
	add := func(name, status, detail string) {
		*checks = append(*checks, doctorCheck{name, status, detail})
	}

	if cfg.Providers.OpenAI == nil {
		add("reasoning effort", statusSkip, "no provider configured")
		return
	}
	effort := cfg.Agents.Defaults.ReasoningEffort
	levels := cfg.Providers.OpenAI.ReasoningLevels
	if effort == "" && len(levels) == 0 {
		add("reasoning effort", statusSkip, "not configured (provider defaults apply)")
		return
	}
	if effort == "" {
		add("reasoning effort", statusOK, fmt.Sprintf("vocabulary: %v", levels))
		return
	}
	if _, ok := providers.NormalizeReasoningEffortIn(effort, levels); ok {
		add("reasoning effort", statusOK, fmt.Sprintf("%q valid for vocabulary %v", effort, levels))
	} else {
		add("reasoning effort", statusFail, fmt.Sprintf("%q not in vocabulary %v — set reasoningLevels to your model's accepted values (see docs/CONFIG.md)", effort, levels))
	}
}

// runDoctor is the `gino doctor` subcommand: post-install health check that
// verifies everything a working Gino needs, without spending tokens.
func runDoctor(homeFlag string) {
	homeDir := resolveHomeDir(homeFlag)
	cfg, cfgErr := config.LoadConfig(homeDir)

	fmt.Printf("🩺 gino doctor — checking %s\n\n", homeDir)

	var checks []doctorCheck
	checkSystem(&checks)
	if cfgErr != nil {
		checks = append(checks, doctorCheck{"config", statusFail, fmt.Sprintf("config.json parse error: %v — fix it, or delete and re-run `gino onboard`", cfgErr)})
	} else {
		checkWorkspace(&checks, homeDir, cfg)
		checkProvider(&checks, cfg)
		checkReasoning(&checks, cfg)
		checkBrain(&checks, cfg)
		checkService(&checks, cfg)
	}

	w := 0
	for _, c := range checks {
		if len(c.name) > w {
			w = len(c.name)
		}
	}
	fails, warns := 0, 0
	for _, c := range checks {
		fmt.Printf("  %s %-*s  %s\n", statusIcon(c.status), w, c.name, c.detail)
		if c.status == statusFail {
			fails++
		}
		if c.status == statusWarn {
			warns++
		}
	}
	fmt.Println()
	switch {
	case fails > 0:
		fmt.Printf("  %d problem(s) found — fix the ✗ items above\n", fails)
		os.Exit(1)
	case warns > 0:
		fmt.Printf("  All checks passed with %d warning(s)\n", warns)
	default:
		fmt.Println("  All checks passed ✓")
	}
}
