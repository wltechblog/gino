package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/wltechblog/gino/internal/audit"
	"github.com/wltechblog/gino/internal/chat"
	"github.com/wltechblog/gino/internal/providers"
	"github.com/wltechblog/gino/internal/tenant"
)

// ============================================================================
// API Server
// ============================================================================
//
// The API server exposes a REST + SSE interface that runs alongside the
// existing channel system (Telegram, Discord). It registers as the "api"
// channel in the hub, meaning the agent loop processes API messages through
// the same pipeline as any other channel.
//
// Architecture:
//
//	Client (mobile/desktop/web)
//	    │
//	    ▼
//	┌───────────────────────────────────┐
//	│  API Server (internal/api)         │
//	│  ├── POST /api/v1/chat/sync        │  ← non-streaming
//	│  ├── GET  /api/v1/chat (SSE)       │  ← streaming connection
//	│  ├── POST /api/v1/chat/stream      │  ← send msg to active stream
//	│  ├── GET  /api/v1/sessions         │  ← session management
//	│  ├── GET  /api/v1/health           │  ← health check
//	│  └── GET  /api/v1/info             │  ← server capabilities
//	└───────────────────────────────────┘
//	    │
//	    ▼
//	┌───────────────────────────────────┐
//	│  Hub (internal/chat)               │
//	│  In ← Inbound{Channel:"api",...}   │
//	│  Out → Outbound{Channel:"api",...} │
//	└───────────────────────────────────┘
//	    │
//	    ▼
//	┌───────────────────────────────────┐
//	│  AgentLoop (internal/agent)        │
//	│  Processes message, runs tools,    │
//	│  sends reply to hub.Out            │
//	└───────────────────────────────────┘
//
// The API server subscribes to the hub for "api" channel messages and routes
// them to the appropriate SSE connection or sync-response channel.
//
// For multi-tenant support, each authenticated user gets their own session
// namespace (api:<userID>:<session>), ensuring conversation isolation.

// ServerConfig holds configuration for the API server.
type ServerConfig struct {
	// Addr is the listen address. Default: ":8443"
	// Use ":443" for production behind a reverse proxy.
	// Behind TLS-terminating reverse proxies (Caddy, nginx), use ":8080".
	Addr string

	// Auth holds authentication settings.
	Auth AuthConfig

	// RequestTimeoutS is the max seconds to wait for an agent response
	// in sync mode. Default: 120
	RequestTimeoutS int

	// Model is the LLM model name (for the /info endpoint).
	Model string

	// MaxIterations is the max tool iterations (for the /info endpoint).
	MaxIterations int

	// VisionSupported indicates whether the configured model supports images.
	VisionSupported bool

	// AdminSecret is the HMAC secret for signing admin UI session cookies.
	// If empty, falls back to hostname-based secret.
	AdminSecret string
}

// Server is the API HTTP server.
type Server struct {
	cfg             ServerConfig
	hub             *chat.Hub
	dispatcher      *Dispatcher
	auth            AuthConfig
	version         string
	startTime       time.Time
	model           string
	maxIterations   int
	visionSupported bool
	tools           map[string]interface{} // tool name -> anything (for /info listing)
	httpServer      *http.Server
	userManager     *tenant.UserManager       // optional: for multi-tenant rate limiting
	store           *tenant.Store             // optional: for persistent admin operations
	auditStore      *audit.Store              // optional: for dashboard token usage summary
	providerMgr     *providers.ProviderManager // optional: for dynamic provider management
	agentLoop       agentLoopMgr              // optional: for hot model updates
}

// agentLoopMgr is the subset of AgentLoop needed for hot provider/model updates.
type agentLoopMgr interface {
	UpdateModel(model string)
	SwapProvider(p providers.LLMProvider)
}

// New creates a new API server wired into the existing hub.
//
// The server does NOT start automatically. Call Start(ctx) to begin listening.
// The hub must already be running (hub.StartRouter called) before processing
// messages from the API channel.
func New(hub *chat.Hub, cfg ServerConfig, version string) *Server {
	s := &Server{
		cfg:             cfg,
		hub:             hub,
		dispatcher:      NewDispatcher(),
		auth:            cfg.Auth,
		version:         version,
		startTime:       time.Now(),
		model:           cfg.Model,
		maxIterations:   cfg.MaxIterations,
		visionSupported: cfg.VisionSupported,
		tools:           make(map[string]interface{}),
	}

	s.routes()
	return s
}

// routes sets up all HTTP routes.
func (s *Server) routes() {
	mux := http.NewServeMux()

	// Public endpoints (no auth required)
	mux.HandleFunc("/api/v1/health", s.handleHealth)
	mux.HandleFunc("/api/v1/info", s.handleInfo)

	// Authenticated endpoints — rate limited on chat endpoints
	mux.HandleFunc("/api/v1/chat/sync", s.authMiddleware(s.rateLimitMiddleware(s.handleChatSync)))
	mux.HandleFunc("/api/v1/chat", s.authMiddleware(s.handleStream))
	mux.HandleFunc("/api/v1/chat/stream", s.authMiddleware(s.rateLimitMiddleware(s.handleStreamSend)))
	mux.HandleFunc("/api/v1/sessions", s.authMiddleware(s.handleSessions))
	mux.HandleFunc("/api/v1/sessions/", s.authMiddleware(s.handleSessionByID))

	// Admin endpoints — require admin privilege
	mux.HandleFunc("/api/v1/admin/users", s.authMiddleware(s.adminMiddleware(s.handleAdminUsers)))
	mux.HandleFunc("/api/v1/admin/users/", s.authMiddleware(s.adminMiddleware(s.handleAdminUser)))
	mux.HandleFunc("/api/v1/admin/tiers", s.authMiddleware(s.adminMiddleware(s.handleAdminTiers)))
	mux.HandleFunc("/api/v1/admin/mcp", s.authMiddleware(s.adminMiddleware(s.handleAdminMCPServers)))
	mux.HandleFunc("/api/v1/admin/mcp/", s.authMiddleware(s.adminMiddleware(s.handleAdminMCPServers)))
	mux.HandleFunc("/api/v1/admin/providers", s.authMiddleware(s.adminMiddleware(s.handleAdminProviders)))
	mux.HandleFunc("/api/v1/admin/providers/", s.authMiddleware(s.adminMiddleware(s.handleAdminProvider)))

	// Admin UI — server-rendered templates
	mux.HandleFunc("/admin/login", s.handleAdminLogin)
	mux.HandleFunc("/admin/logout", s.handleAdminLogout)
	mux.HandleFunc("/admin/users/action", s.adminUIMiddleware(s.handleAdminActionUser))
	mux.HandleFunc("/admin/tiers/action", s.adminUIMiddleware(s.handleAdminActionTier))
	mux.HandleFunc("/admin/mcp/action", s.adminUIMiddleware(s.handleAdminActionMCP))
	mux.HandleFunc("/admin/providers/action", s.adminUIMiddleware(s.handleAdminActionProvider))
	mux.HandleFunc("/admin/users/", s.adminUIMiddleware(s.handleAdminUIUserEdit))
	mux.HandleFunc("/admin/users", s.adminUIMiddleware(s.handleAdminUIUsers))
	mux.HandleFunc("/admin/tiers/", s.adminUIMiddleware(s.handleAdminUITierEdit))
	mux.HandleFunc("/admin/tiers", s.adminUIMiddleware(s.handleAdminUITiers))
	mux.HandleFunc("/admin/mcp", s.adminUIMiddleware(s.handleAdminUIMCP))
	mux.HandleFunc("/admin/providers/", s.adminUIMiddleware(s.handleAdminUIProviderEdit))
	mux.HandleFunc("/admin/providers", s.adminUIMiddleware(s.handleAdminUIProviders))
	mux.HandleFunc("/admin/", s.adminUIMiddleware(s.handleAdminDashboard))
	mux.HandleFunc("/admin", s.adminUIMiddleware(s.handleAdminDashboard))

	// Root redirect for browsers
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<!DOCTYPE html>
<html><body>
<h1>Gino API</h1>
<p>Endpoints:</p>
<ul>
<li>GET <code>/api/v1/health</code></li>
<li>GET <code>/api/v1/info</code></li>
<li>POST <code>/api/v1/chat/sync</code></li>
<li>GET <code>/api/v1/chat</code> (SSE stream)</li>
<li>POST <code>/api/v1/chat/stream</code></li>
<li>GET <code>/api/v1/sessions</code></li>
</ul>
</body></html>`))
			return
		}
		http.NotFound(w, r)
	})

	s.httpServer = &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.corsMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// Start begins listening for HTTP requests. Blocks until the context is
// cancelled or the server encounters a fatal error.
func (s *Server) Start(ctx context.Context) error {
	addr := s.cfg.Addr
	if addr == "" {
		addr = ":8443"
	}
	s.httpServer.Addr = addr

	// Subscribe to the hub for "api" channel responses
	go s.routeHubResponses(ctx)

	log.Printf("API: listening on %s", addr)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.httpServer.Shutdown(shutdownCtx)
	}()

	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// routeHubResponses reads outbound messages from the hub and dispatches
// them to SSE connections based on the ChatID.
//
// The API server subscribes to the "api" channel on the hub. Outbound messages
// targeting the api channel are dispatched by ChatID to the correct SSE
// connection (or sync-response channel).
func (s *Server) routeHubResponses(ctx context.Context) {
	sub := s.hub.Subscribe("api")

	for {
		select {
		case <-ctx.Done():
			return
		case out, ok := <-sub:
			if !ok {
				return
			}
			// Route by ChatID — the ChatID is the SSE connection ID
			// or a sync-request identifier.
			conn := s.dispatcher.get(out.ChatID)
			if conn != nil {
				select {
				case conn.ch <- out:
				default:
					log.Printf("API: SSE connection %s buffer full, dropping message", out.ChatID)
				}
			} else {
				log.Printf("API: no SSE connection for ChatID %s, dropping outbound message", out.ChatID)
			}
		}
	}
}

// corsMiddleware adds CORS headers for browser-based clients.
// For native apps (mobile/desktop), CORS is irrelevant but harmless.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Last-Event-ID")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// SetTools populates the tool list for the /info endpoint.
// Called after the agent loop registers its tools.
func (s *Server) SetTools(names []string) {
	s.tools = make(map[string]interface{}, len(names))
	for _, n := range names {
		s.tools[n] = true
	}
}

// SetUserManager wires the tenant user manager for multi-tenant rate limiting.
// When set, chat endpoints enforce per-tier rate limits and concurrency caps.
func (s *Server) SetUserManager(um *tenant.UserManager) {
	s.userManager = um
}

// SetStore wires the tenant store for persistent admin operations.
func (s *Server) SetStore(store *tenant.Store) {
	s.store = store
}

// SetAuditStore wires the audit store for dashboard summaries.
func (s *Server) SetAuditStore(store *audit.Store) {
	s.auditStore = store
}

// SetProviderManager wires the dynamic provider manager.
func (s *Server) SetProviderManager(pm *providers.ProviderManager) {
	s.providerMgr = pm
}

// SetAgentLoop wires the agent loop for hot provider/model updates.
func (s *Server) SetAgentLoop(a agentLoopMgr) {
	s.agentLoop = a
}

// checkRateLimit verifies the user's tier allows a new turn.
// Returns true if allowed, or sends a 429 and returns false.
// Must be called AFTER authentication (userID is in context).
func (s *Server) checkRateLimit(w http.ResponseWriter, r *http.Request) bool {
	if s.userManager == nil {
		return true // single-tenant mode, no limits
	}

	userID := userIDFromRequest(r)
	uctx := s.userManager.Get(userID)
	if uctx == nil {
		return true // unknown user, allow (auth already passed)
	}

	allowed, reason := uctx.CanStartTurn()
	if !allowed {
		retryAfter := 60 // default 60s
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
		s.writeError(w, http.StatusTooManyRequests,
			"Rate limit exceeded: "+reason)
		return false
	}

	// Reserve the turn
	uctx.BeginTurn()

	// Use a done channel to ensure EndTurn is called exactly once
	// via response writer wrapper or defer in the handler.
	// We store it in context for the handler to call.
	ctx := context.WithValue(r.Context(), rateLimitEndKey, func() {
		uctx.EndTurn()
	})
	*r = *r.WithContext(ctx)
	return true
}

// endRateLimitedTurn calls the EndTurn callback if one was registered in context.
func endRateLimitedTurn(r *http.Request) {
	if fn, ok := r.Context().Value(rateLimitEndKey).(func()); ok {
		fn()
	}
}

// rateLimitMiddleware enforces per-user rate limits before processing.
// Must be applied AFTER authMiddleware (needs userID in context).
func (s *Server) rateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkRateLimit(w, r) {
			return
		}
		defer endRateLimitedTurn(r)
		next(w, r)
	}
}
