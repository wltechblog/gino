package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wltechblog/gino/internal/agent"
	"github.com/wltechblog/gino/internal/agent/memory"
	"github.com/wltechblog/gino/internal/channels"
	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/cron"
	"github.com/wltechblog/gino/internal/heartbeat"
	"github.com/wltechblog/gino/internal/providers"
	picosignal "github.com/wltechblog/gino/internal/signal"
	"github.com/wltechblog/gino/internal/tui"
)

const version = "0.4.0"

// resolveHomeDir resolves the gino home directory.
func resolveHomeDir(homeFlag string) string {
	if homeFlag == "" {
		userHome, _ := os.UserHomeDir()
		return filepath.Join(userHome, ".gino")
	}
	if strings.HasPrefix(homeFlag, "~/") {
		userHome, _ := os.UserHomeDir()
		return filepath.Join(userHome, homeFlag[2:])
	}
	return homeFlag
}

func expandWorkspace(ws, homeDir string) string {
	if ws == "" {
		return filepath.Join(homeDir, "workspace")
	}
	if strings.HasPrefix(ws, "~/") {
		userHome, _ := os.UserHomeDir()
		return filepath.Join(userHome, ws[2:])
	}
	return ws
}

func usage() {
	fmt.Fprintf(os.Stderr, "gino v%s — lightweight agent runtime\n\n", version)
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  gino <command> [flags]\n\n")
	fmt.Fprintf(os.Stderr, "Commands:\n")
	fmt.Fprintf(os.Stderr, "  version          Print version\n")
	fmt.Fprintf(os.Stderr, "  onboard          Create default config and workspace\n")
	fmt.Fprintf(os.Stderr, "  channels login   Interactively connect a channel\n")
	fmt.Fprintf(os.Stderr, "  agent            Run a single-shot agent query\n")
	fmt.Fprintf(os.Stderr, "  chat             Start interactive TUI chat session\n")
	fmt.Fprintf(os.Stderr, "  gateway          Start long-running gateway\n")
	fmt.Fprintf(os.Stderr, "  signal send      Send an external signal to a running gateway\n")
	fmt.Fprintf(os.Stderr, "  memory read      Read memory (today or long)\n")
	fmt.Fprintf(os.Stderr, "  memory append    Append content to memory\n")
	fmt.Fprintf(os.Stderr, "  memory write     Overwrite long-term memory\n")
	fmt.Fprintf(os.Stderr, "  memory recent    Show recent days' notes\n")
	fmt.Fprintf(os.Stderr, "  memory rank      Rank memories by relevance\n\n")
	fmt.Fprintf(os.Stderr, "Global flags:\n")
	fmt.Fprintf(os.Stderr, "  -home string     gino home directory (default: ~/.gino)\n")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	command := os.Args[1]
	// Extract -home from args manually so that subcommand-specific flags
	// (e.g. agent -m) are not consumed by the global flag parser.
	args := os.Args[2:]
	homeVal := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-home" && i+1 < len(args) {
			homeVal = args[i+1]
			i++ // skip value
			continue
		}
		if strings.HasPrefix(args[i], "-home=") {
			homeVal = strings.TrimPrefix(args[i], "-home=")
			continue
		}
		rest = append(rest, args[i])
	}
	homeFlag := homeVal

	switch command {
	case "version":
		fmt.Printf("🤖 gino v%s\n", version)

	case "onboard":
		runOnboard(homeFlag)

	case "channels":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "Usage: gino channels login")
			os.Exit(2)
		}
		switch rest[0] {
		case "login":
			runChannelsLogin(homeFlag)
		default:
			fmt.Fprintf(os.Stderr, "unknown channels subcommand: %s\n", rest[0])
			os.Exit(2)
		}

	case "agent":
		runAgent(homeFlag, rest)

	case "chat":
		runChat(homeFlag, rest)

	case "gateway":
		runGateway(homeFlag, rest)

	case "signal":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "Usage: gino signal send [flags]")
			os.Exit(2)
		}
		switch rest[0] {
		case "send":
			runSignalSend(rest[1:])
		default:
			fmt.Fprintf(os.Stderr, "unknown signal subcommand: %s\n", rest[0])
			os.Exit(2)
		}

	case "memory":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "Usage: gino memory <read|append|write|recent|rank> [flags]")
			os.Exit(2)
		}
		switch rest[0] {
		case "read":
			runMemoryRead(homeFlag, rest[1:])
		case "append":
			runMemoryAppend(homeFlag, rest[1:])
		case "write":
			runMemoryWrite(homeFlag, rest[1:])
		case "recent":
			runMemoryRecent(homeFlag, rest[1:])
		case "rank":
			runMemoryRank(homeFlag, rest[1:])
		default:
			fmt.Fprintf(os.Stderr, "unknown memory subcommand: %s\n", rest[0])
			os.Exit(2)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", command)
		usage()
		os.Exit(2)
	}
}

// ─── onboard ────────────────────────────────────────────────────────────────

func runOnboard(homeFlag string) {
	homeDir := resolveHomeDir(homeFlag)
	cfgPath, workspacePath, err := config.Onboard(homeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "onboard failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Wrote config to %s\nInitialized workspace at %s\n", cfgPath, workspacePath)
}

// ─── channels login ─────────────────────────────────────────────────────────

func runChannelsLogin(homeFlag string) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("Which channel would you like to connect?")
	fmt.Println()
	fmt.Println("  1) Telegram")
	fmt.Println("  2) Discord")
	fmt.Println()
	fmt.Print("Enter 1 or 2: ")

	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(strings.ToLower(choice))

	homeDir := resolveHomeDir(homeFlag)
	cfg, err := config.LoadConfig(homeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	cfgPath, _, err := config.ResolvePaths(homeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to resolve config path: %v\n", err)
		os.Exit(1)
	}

	switch choice {
	case "1", "telegram":
		setupTelegramInteractive(reader, cfg, cfgPath)
	case "2", "discord":
		setupDiscordInteractive(reader, cfg, cfgPath)
	default:
		fmt.Fprintf(os.Stderr, "invalid choice %q — please enter 1 or 2\n", choice)
		os.Exit(2)
	}
}

// ─── agent ──────────────────────────────────────────────────────────────────

func runAgent(homeFlag string, args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	msg := fs.String("m", "", "Message to send to the agent")
	modelFlag := fs.String("M", "", "Model to use (overrides config/provider default)")
	sessionKey := fs.String("session", "", "Session key for multi-turn context persistence")
	systemPromptOverride := fs.String("system-prompt", "", "Override the system prompt (used by benchmarks)")
	_ = fs.Parse(args)

	if *msg == "" {
		fmt.Fprintln(os.Stderr, "Specify a message with -m \"your message\"")
		os.Exit(2)
	}

	homeDir := resolveHomeDir(homeFlag)
	hub := chat.NewHub(100)
	cfg, _ := config.LoadConfig(homeDir)
	provider := providers.NewProviderFromConfig(cfg)

	model := *modelFlag
	if model == "" && cfg.Agents.Defaults.Model != "" {
		model = cfg.Agents.Defaults.Model
	}
	if model == "" {
		model = provider.GetDefaultModel()
	}

	maxIter := cfg.Agents.Defaults.MaxToolIterations
	if maxIter <= 0 {
		maxIter = 100
	}
	ws := expandWorkspace(cfg.Agents.Defaults.Workspace, homeDir)
	if err := os.Chdir(ws); err != nil {
		fmt.Fprintf(os.Stderr, "failed to chdir to workspace %q: %v\n", ws, err)
		os.Exit(1)
	}
	ag := agent.NewAgentLoop(hub, provider, model, maxIter, ws, nil, cfg.MCPServers, cfg.Agents.Defaults.AllowedDirs, cfg.Agents.Defaults.DisableTools, cfg.Brain, homeDir, cfg.Agents.Defaults.Sandbox, "", cfg.Agents.Defaults.MaxTurnMessages, cfg.Agents.Defaults.MaxToolResultChars, cfg.Agents.Defaults.Compaction, cfg.Agents.Defaults.Web)
	defer ag.Close()
	if cfg.Agents.Defaults.EnableToolActivityIndicator != nil {
		ag.SetToolActivityIndicator(*cfg.Agents.Defaults.EnableToolActivityIndicator)
	}
	if cfg.Agents.Defaults.EnableToolCallMessages != nil {
		ag.SetToolCallMessages(*cfg.Agents.Defaults.EnableToolCallMessages)
	}

	// Use requestTimeoutS from config, fallback to 300s
	cliTimeout := 300 * time.Second
	if cfg.Agents.Defaults.RequestTimeoutS > 0 {
		cliTimeout = time.Duration(cfg.Agents.Defaults.RequestTimeoutS) * time.Second
	}
	resp, err := ag.ProcessDirectWithSessionAndSystemPrompt(*msg, cliTimeout, *sessionKey, *systemPromptOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println(resp)
}

// ─── chat (TUI) ─────────────────────────────────────────────────────────────

func runChat(homeFlag string, args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	modelFlag := fs.String("M", "", "Model to use (overrides config/provider default)")
	_ = fs.Parse(args)

	homeDir := resolveHomeDir(homeFlag)
	cfg, err := config.LoadConfig(homeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	provider := providers.NewProviderFromConfig(cfg)

	ws := expandWorkspace(cfg.Agents.Defaults.Workspace, homeDir)
	if err := os.Chdir(ws); err != nil {
		fmt.Fprintf(os.Stderr, "failed to chdir to workspace %q: %v\n", ws, err)
		os.Exit(1)
	}

	session := tui.New(cfg, provider, homeDir, ws)
	if *modelFlag != "" {
		session.Model = *modelFlag
	}
	ctx := context.Background()
	if err := session.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "chat error: %v\n", err)
		os.Exit(1)
	}
}

// ─── gateway ────────────────────────────────────────────────────────────────

func runGateway(homeFlag string, args []string) {
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	modelFlag := fs.String("M", "", "Model to use (overrides config/provider default)")
	_ = fs.Parse(args)

	homeDir := resolveHomeDir(homeFlag)
	hub := chat.NewHub(200)
	cfg, _ := config.LoadConfig(homeDir)
	provider := providers.NewProviderFromConfig(cfg)

	model := *modelFlag
	if model == "" && cfg.Agents.Defaults.Model != "" {
		model = cfg.Agents.Defaults.Model
	}
	if model == "" {
		model = provider.GetDefaultModel()
	}

	scheduler := cron.NewScheduler(func(job cron.Job) {
		log.Printf("cron fired: %s — %s", job.Name, job.Message)
		hub.In <- chat.Inbound{
			Channel:  job.Channel,
			SenderID: "cron",
			ChatID:   job.ChatID,
			Content:  fmt.Sprintf("[Scheduled reminder fired] %s — Please relay this to the user in a friendly way.", job.Message),
		}
	})
	if err := scheduler.SetPersistencePath(filepath.Join(homeDir, "cron_jobs.json")); err != nil {
		log.Printf("cron: failed to set persistence path: %v", err)
	}

	maxIter := cfg.Agents.Defaults.MaxToolIterations
	if maxIter <= 0 {
		maxIter = 100
	}
	ws := expandWorkspace(cfg.Agents.Defaults.Workspace, homeDir)
	if err := os.Chdir(ws); err != nil {
		fmt.Fprintf(os.Stderr, "failed to chdir to workspace %q: %v\n", ws, err)
		os.Exit(1)
	}
	// Compute signal socket path for MCP env injection
	signalSocketPath := ""
	if cfg.Signal.Enabled {
		signalSocketPath = cfg.Signal.GetSocketPath(homeDir, ws)
	}
	ag := agent.NewAgentLoop(hub, provider, model, maxIter, ws, scheduler, cfg.MCPServers, cfg.Agents.Defaults.AllowedDirs, cfg.Agents.Defaults.DisableTools, cfg.Brain, homeDir, cfg.Agents.Defaults.Sandbox, signalSocketPath, cfg.Agents.Defaults.MaxTurnMessages, cfg.Agents.Defaults.MaxToolResultChars, cfg.Agents.Defaults.Compaction, cfg.Agents.Defaults.Web)
	defer ag.Close()
	if cfg.Agents.Defaults.EnableToolActivityIndicator != nil {
		ag.SetToolActivityIndicator(*cfg.Agents.Defaults.EnableToolActivityIndicator)
	}
	if cfg.Agents.Defaults.EnableToolCallMessages != nil {
		ag.SetToolCallMessages(*cfg.Agents.Defaults.EnableToolCallMessages)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ag.Run(ctx)
	go scheduler.Start(ctx.Done())

	hbInterval := time.Duration(cfg.Agents.Defaults.HeartbeatIntervalS) * time.Second
	if hbInterval <= 0 {
		hbInterval = 60 * time.Second
	}
	heartbeat.StartHeartbeat(ctx, ws, hbInterval, hub)

	// Start external signal listener (Unix domain socket)
	if cfg.Signal.Enabled {
		socketPath := cfg.Signal.GetSocketPath(homeDir, ws)
		sigRegistry := picosignal.NewRegistry(cfg.Signal.Actions)
		sigListener := picosignal.NewListener(socketPath, hub, sigRegistry, cfg.Signal.DefaultChannel, cfg.Signal.DefaultChatID)
		go func() {
			if err := sigListener.Start(ctx); err != nil {
				log.Printf("Signal: listener error: %v", err)
			}
		}()
		log.Printf("Signal: external trigger system enabled on %s", socketPath)
		ag.SetSignalListener(sigListener)
	}

	if cfg.Channels.Telegram.Enabled {
		showTyping := cfg.Agents.Defaults.EnableToolActivityIndicator == nil || *cfg.Agents.Defaults.EnableToolActivityIndicator
		if err := channels.StartTelegram(ctx, hub, cfg.Channels.Telegram.Token, cfg.Channels.Telegram.AllowFrom, showTyping, ws); err != nil {
			log.Fatalf("Telegram: %v", err)
		}
	}

	if cfg.Channels.Discord.Enabled {
		rl := channels.DiscordRateLimit{
			PerMinute: cfg.Channels.Discord.RateLimitPerMinute,
			PerHour:   cfg.Channels.Discord.RateLimitPerHour,
			TotalHour: cfg.Channels.Discord.RateLimitTotalHour,
		}
		if err := channels.StartDiscord(ctx, hub, cfg.Channels.Discord.Token, cfg.Channels.Discord.AllowFrom, cfg.Channels.Discord.AllowDMs, cfg.Channels.Discord.MonitorChannels, rl); err != nil {
			log.Fatalf("Discord: %v", err)
		}
	}

	// Start the router AFTER all subscribers are registered.
	// IMPORTANT: Do NOT read from hub.Out directly anywhere else — the router
	// is the sole consumer of hub.Out and dispatches to channel subscribers.
	hub.StartRouter(ctx)

	log.Println("gateway started — waiting for messages")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("shutting down...")
}

// ─── signal send ────────────────────────────────────────────────────────────

func runSignalSend(args []string) {
	fs := flag.NewFlagSet("signal send", flag.ExitOnError)
	sigSource := fs.String("s", "", "Source identifier (e.g., my-script, camera-1) (required)")
	sigAction := fs.String("a", "", "Registered action name (required)")
	sigChannel := fs.String("c", "", "Target channel (e.g., telegram, discord)")
	sigChatID := fs.String("chat-id", "", "Target chat ID")
	socketPath := fs.String("socket", "", "Unix socket path (default: {workspace}/.gino/signals.sock)")
	// Also accept --source and --action long forms
	fs.StringVar(sigSource, "source", "", "Source identifier")
	fs.StringVar(sigAction, "action", "", "Registered action name")
	_ = fs.Parse(args)

	if *sigAction == "" {
		fmt.Fprintln(os.Stderr, "action is required (--action or -a)")
		os.Exit(2)
	}
	if *sigSource == "" {
		fmt.Fprintln(os.Stderr, "source is required (--source or -s)")
		os.Exit(2)
	}

	sig := picosignal.Signal{
		Source:  *sigSource,
		Action:  *sigAction,
		Channel: *sigChannel,
		ChatID:  *sigChatID,
	}

	if err := picosignal.SendSignal(*socketPath, sig); err != nil {
		fmt.Fprintf(os.Stderr, "failed to send signal: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Signal sent: source=%s action=%s\n", *sigSource, *sigAction)
}

// ─── memory read ────────────────────────────────────────────────────────────

func runMemoryRead(homeFlag string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: gino memory read <today|long>")
		os.Exit(2)
	}
	target := args[0]
	homeDir := resolveHomeDir(homeFlag)
	cfg, _ := config.LoadConfig(homeDir)
	ws := expandWorkspace(cfg.Agents.Defaults.Workspace, homeDir)
	mem := memory.NewMemoryStoreWithWorkspace(ws, 100)

	switch target {
	case "today":
		out, _ := mem.ReadToday()
		fmt.Println(out)
	case "long":
		out, _ := mem.ReadLongTerm()
		fmt.Println(out)
	default:
		fmt.Fprintln(os.Stderr, "unknown target: "+target)
		os.Exit(2)
	}
}

// ─── memory append ──────────────────────────────────────────────────────────

func runMemoryAppend(homeFlag string, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: gino memory append <today|long> -c <content>")
		os.Exit(2)
	}
	target := args[0]

	fs := flag.NewFlagSet("memory append", flag.ExitOnError)
	content := fs.String("c", "", "Content to append")
	_ = fs.Parse(args[1:])

	if *content == "" {
		fmt.Fprintln(os.Stderr, "-c content required")
		os.Exit(2)
	}

	homeDir := resolveHomeDir(homeFlag)
	cfg, _ := config.LoadConfig(homeDir)
	ws := expandWorkspace(cfg.Agents.Defaults.Workspace, homeDir)
	mem := memory.NewMemoryStoreWithWorkspace(ws, 100)

	switch target {
	case "today":
		if err := mem.AppendToday(*content); err != nil {
			fmt.Fprintln(os.Stderr, "append failed:", err)
			os.Exit(1)
		}
		fmt.Println("appended to today")
	case "long":
		lt, err := mem.ReadLongTerm()
		if err != nil {
			fmt.Fprintln(os.Stderr, "append long failed:", err)
			os.Exit(1)
		}
		if err := mem.WriteLongTerm(lt + "\n" + *content); err != nil {
			fmt.Fprintln(os.Stderr, "append long failed:", err)
			os.Exit(1)
		}
		fmt.Println("appended to long-term memory")
	default:
		fmt.Fprintln(os.Stderr, "unknown target:", target)
		os.Exit(2)
	}
}

// ─── memory write ───────────────────────────────────────────────────────────

func runMemoryWrite(homeFlag string, args []string) {
	if len(args) < 1 || args[0] != "long" {
		fmt.Fprintln(os.Stderr, "Usage: gino memory write long -c <content>")
		os.Exit(2)
	}

	fs := flag.NewFlagSet("memory write", flag.ExitOnError)
	content := fs.String("c", "", "Content to write")
	_ = fs.Parse(args[1:])

	if *content == "" {
		fmt.Fprintln(os.Stderr, "-c content required")
		os.Exit(2)
	}

	homeDir := resolveHomeDir(homeFlag)
	cfg, _ := config.LoadConfig(homeDir)
	ws := expandWorkspace(cfg.Agents.Defaults.Workspace, homeDir)
	mem := memory.NewMemoryStoreWithWorkspace(ws, 100)

	if err := mem.WriteLongTerm(*content); err != nil {
		fmt.Fprintln(os.Stderr, "write failed:", err)
		os.Exit(1)
	}
	fmt.Println("wrote long-term memory")
}

// ─── memory recent ──────────────────────────────────────────────────────────

func runMemoryRecent(homeFlag string, args []string) {
	fs := flag.NewFlagSet("memory recent", flag.ExitOnError)
	days := fs.Int("d", 1, "Number of days to include")
	_ = fs.Parse(args)

	homeDir := resolveHomeDir(homeFlag)
	cfg, _ := config.LoadConfig(homeDir)
	ws := expandWorkspace(cfg.Agents.Defaults.Workspace, homeDir)
	mem := memory.NewMemoryStoreWithWorkspace(ws, 100)

	out, _ := mem.GetRecentMemories(*days)
	fmt.Println(out)
}

// ─── memory rank ────────────────────────────────────────────────────────────

func runMemoryRank(homeFlag string, args []string) {
	fs := flag.NewFlagSet("memory rank", flag.ExitOnError)
	q := fs.String("q", "", "Query to rank memories against")
	top := fs.Int("k", 5, "Number of top memories to show")
	verbose := fs.Bool("v", false, "Enable verbose diagnostic logging")
	_ = fs.Parse(args)

	if *q == "" {
		fmt.Fprintln(os.Stderr, "-q query required")
		os.Exit(2)
	}

	homeDir := resolveHomeDir(homeFlag)
	cfg, _ := config.LoadConfig(homeDir)
	ws := expandWorkspace(cfg.Agents.Defaults.Workspace, homeDir)
	mem := memory.NewMemoryStoreWithWorkspace(ws, 100)

	items := make([]memory.MemoryItem, 0)
	if td, err := mem.ReadToday(); err == nil && td != "" {
		for _, line := range strings.Split(td, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if idx := strings.Index(line, "] "); idx != -1 && strings.HasPrefix(line, "[") {
				line = strings.TrimSpace(line[idx+2:])
			}
			items = append(items, memory.MemoryItem{Kind: "today", Text: line})
		}
	}
	if lt, err := mem.ReadLongTerm(); err == nil && lt != "" {
		for _, line := range strings.Split(lt, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			items = append(items, memory.MemoryItem{Kind: "long", Text: line})
		}
	}

	provider := providers.NewProviderFromConfig(cfg)
	var logger *log.Logger
	if *verbose {
		logger = log.New(os.Stdout, "ranker: ", 0)
	}
	ranker := memory.NewLLMRankerWithLogger(provider, provider.GetDefaultModel(), logger)
	res := ranker.Rank(*q, items, *top)
	for i, m := range res {
		fmt.Printf("%d: %s (%s)\n", i+1, m.Text, m.Kind)
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

func promptLine(reader *bufio.Reader, prompt string) string {
	fmt.Print(prompt)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

func parseAllowFrom(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}

func setupTelegramInteractive(reader *bufio.Reader, cfg config.Config, cfgPath string) {
	fmt.Println()
	fmt.Println("=== Telegram Setup ===")
	fmt.Println()
	fmt.Println("You need a bot token from @BotFather on Telegram:")
	fmt.Println("  1. Message @BotFather on Telegram")
	fmt.Println("  2. Send /newbot and follow the prompts")
	fmt.Println("  3. Copy the token it gives you")
	fmt.Println()

	token := promptLine(reader, "Bot token: ")
	if token == "" {
		fmt.Fprintln(os.Stderr, "error: token cannot be empty")
		return
	}

	fmt.Println()
	fmt.Println("To restrict who can message your bot, enter your Telegram user ID.")
	fmt.Println("Find it by messaging @userinfobot on Telegram.")
	fmt.Println("Leave blank to allow everyone.")
	fmt.Println()

	allowFromStr := promptLine(reader, "Allowed user IDs (comma-separated, blank = everyone): ")
	allowFrom := parseAllowFrom(allowFromStr)

	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.Token = token
	cfg.Channels.Telegram.AllowFrom = allowFrom

	if err := config.SaveConfig(cfg, cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "failed to save config: %v\n", err)
		return
	}

	fmt.Println()
	fmt.Println("Telegram configured! Run 'gino gateway' to start.")
}

func setupDiscordInteractive(reader *bufio.Reader, cfg config.Config, cfgPath string) {
	fmt.Println()
	fmt.Println("=== Discord Setup ===")
	fmt.Println()
	fmt.Println("You need a bot token from the Discord Developer Portal:")
	fmt.Println("  1. Go to https://discord.com/developers/applications")
	fmt.Println("  2. Create an application → Bot → Reset Token")
	fmt.Println("  3. Enable \"Message Content Intent\" under Privileged Gateway Intents")
	fmt.Println("  4. Invite the bot to your server via OAuth2 → URL Generator")
	fmt.Println("  5. Copy the token and paste it below")
	fmt.Println()

	token := promptLine(reader, "Bot token: ")
	if token == "" {
		fmt.Fprintln(os.Stderr, "error: token cannot be empty")
		return
	}

	fmt.Println()
	fmt.Println("To restrict who can message your bot, enter Discord user IDs.")
	fmt.Println("Enable Developer Mode (Settings → Advanced) then right-click your name → Copy User ID.")
	fmt.Println("Leave blank to allow everyone.")
	fmt.Println()

	allowFromStr := promptLine(reader, "Allowed user IDs (comma-separated, blank = everyone): ")
	allowFrom := parseAllowFrom(allowFromStr)

	cfg.Channels.Discord.Enabled = true
	cfg.Channels.Discord.Token = token
	cfg.Channels.Discord.AllowFrom = allowFrom

	if err := config.SaveConfig(cfg, cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "failed to save config: %v\n", err)
		return
	}

	fmt.Println()
	fmt.Println("Discord configured! Run 'gino gateway' to start.")
}
