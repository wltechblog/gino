package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
	"github.com/wltechblog/gino/internal/tenant"
)

// ============================================================================
// Admin UI — Server-Rendered Templates
// ============================================================================
//
// The admin UI provides a web interface for managing users, tiers, and MCP
// servers. It uses Go html/template with embedded templates — no external
// dependencies, served from the Go binary.
//
// Authentication: admin enters their token at /admin/login. A signed session
// cookie is set. All /admin/ routes require a valid session.

const adminCookieName = "gino_admin_session"
const adminCookieTTL = 24 * time.Hour

// ─── Session Management ───────────────────────────────────────────────────────

// createAdminSession creates a signed session cookie value.
func (s *Server) createAdminSession(userID string) string {
	payload := fmt.Sprintf("%s|%d", userID, time.Now().Unix())
	sig := s.signSession(payload)
	return payload + "." + sig
}

// validateAdminSession validates a session cookie and returns the user ID.
func (s *Server) validateAdminSession(cookieValue string) string {
	parts := strings.SplitN(cookieValue, ".", 2)
	if len(parts) != 2 {
		return ""
	}
	payload := parts[0]
	sig := parts[1]
	expectedSig := s.signSession(payload)
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return ""
	}
	// Extract user ID
	fields := strings.SplitN(payload, "|", 2)
	if len(fields) != 2 {
		return ""
	}
	return fields[0]
}

// signSession creates an HMAC signature for a session payload.
func (s *Server) signSession(payload string) string {
	secret := s.adminSecret()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// adminSecret returns the secret used for signing sessions.
// Falls back to a process-level secret if not configured.
func (s *Server) adminSecret() string {
	if s.cfg.AdminSecret != "" {
		return s.cfg.AdminSecret
	}
	// Fall back to hostname + PID for some uniqueness
	hostname, _ := os.Hostname()
	return "gino-" + hostname
}

// ─── Admin UI Middleware ──────────────────────────────────────────────────────

// adminUIMiddleware checks for a valid admin session cookie.
func (s *Server) adminUIMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.userManager == nil {
			http.Error(w, "Multi-tenant mode not enabled", http.StatusServiceUnavailable)
			return
		}
		cookie, err := r.Cookie(adminCookieName)
		if err != nil {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		userID := s.validateAdminSession(cookie.Value)
		if userID == "" {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		uctx := s.userManager.Get(userID)
		if uctx == nil || !uctx.IsAdmin() {
			http.Redirect(w, r, "/admin/login?error=not_admin", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), userIDKey, userID)
		r = r.WithContext(ctx)
		next(w, r)
	}
}

// ─── Login ────────────────────────────────────────────────────────────────────

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		data := adminPageData{
			Title:   "Admin Login",
			Active:  "login",
			BaseURL: s.adminBaseURL(r),
		}
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			data.ErrorMessage = mapError(errMsg)
		}
		s.renderAdmin(w, "login", data)
		return
	}

	if r.Method == http.MethodPost {
		token := r.FormValue("token")
		if token == "" {
			http.Redirect(w, r, "/admin/login?error=missing_token", http.StatusSeeOther)
			return
		}

		uctx, _ := s.userManager.GetByToken(token)
		if uctx == nil || !uctx.IsAdmin() {
			http.Redirect(w, r, "/admin/login?error=invalid_token", http.StatusSeeOther)
			return
		}

		// Set session cookie
		session := s.createAdminSession(uctx.Config.ID)
		http.SetCookie(w, &http.Cookie{
			Name:     adminCookieName,
			Value:    session,
			Path:     "/admin",
			MaxAge:   int(adminCookieTTL.Seconds()),
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})

		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
	}
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
	})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// ─── Dashboard ────────────────────────────────────────────────────────────────

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	users := s.userManager.AllUsers()
	tiers := s.userManager.AllTiers()

	dashboardData := dashboardData{
		Users:     len(users),
		Tiers:     len(tiers),
		Admins:    countAdmins(users),
		ActiveNow: countActive(users, 5*time.Minute),
	}

	// Token usage summary
	if s.auditStore != nil {
		if summary, err := s.auditStore.QueryUsageSummary("", time.Now().AddDate(0, 0, -30)); err == nil && summary != nil {
			dashboardData.TokenSummary = json.RawMessage("{}") // summary available
		}
	}

	// MCP server count
	if s.store != nil {
		if servers, err := s.store.LoadAllMCPServers(); err == nil {
			dashboardData.MCPServers = len(servers)
		}
	}

	// Provider count
	if s.providerMgr != nil {
		dashboardData.Providers = len(s.providerMgr.ListProviders())
	}

	data := adminPageData{
		Title:       "Dashboard",
		Active:      "dashboard",
		BaseURL:     s.adminBaseURL(r),
		Dashboard:   &dashboardData,
		CurrentTime: time.Now(),
	}
	s.renderAdmin(w, "dashboard", data)
}

// ─── Users ────────────────────────────────────────────────────────────────────

func (s *Server) handleAdminUIUsers(w http.ResponseWriter, r *http.Request) {
	users := s.userManager.AllUsers()
	tiers := s.userManager.AllTiers()

	userList := make([]userRowData, 0, len(users))
	for _, u := range users {
		userList = append(userList, userRowData{
			ID:          u.ID,
			DisplayName: u.DisplayName,
			Tier:        u.Tier,
			Admin:       u.Admin,
			Channels:    u.Channels,
			Workspace:   u.WorkspaceOverride,
			CreatedAt:   u.CreatedAt,
		})
	}

	tierNames := make([]string, 0, len(tiers))
	for _, t := range tiers {
		tierNames = append(tierNames, t.Name)
	}

	// Check for created-user token flash
	createdID := r.URL.Query().Get("created")
	createdToken := r.URL.Query().Get("token")

	data := adminPageData{
		Title:     "Users",
		Active:    "users",
		BaseURL:   s.adminBaseURL(r),
		Users:     userList,
		TierNames: tierNames,
	}
	if createdID != "" && createdToken != "" {
		data.SuccessMsg = "User \"" + createdID + "\" created. Token: " + createdToken
	}
	s.renderAdmin(w, "users", data)
}

func (s *Server) handleAdminUIUserEdit(w http.ResponseWriter, r *http.Request) {
	userID := strings.TrimPrefix(r.URL.Path, "/admin/users/")
	if userID == "" {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	uctx := s.userManager.Get(userID)
	if uctx == nil {
		s.renderAdminError(w, "User not found: "+userID)
		return
	}

	tiers := s.userManager.AllTiers()
	tierNames := make([]string, 0, len(tiers))
	for _, t := range tiers {
		tierNames = append(tierNames, t.Name)
	}

	data := adminPageData{
		Title:    "Edit User",
		Active:   "users",
		BaseURL:  s.adminBaseURL(r),
		EditUser: &userRowData{
			ID:          uctx.Config.ID,
			DisplayName: uctx.Config.DisplayName,
			Tier:        uctx.Config.Tier,
			Admin:       uctx.Config.Admin,
			Channels:    uctx.Config.Channels,
			Workspace:   uctx.Config.WorkspaceOverride,
		},
		TierNames: tierNames,
	}
	s.renderAdmin(w, "user_edit", data)
}

// ─── Tiers ────────────────────────────────────────────────────────────────────

func (s *Server) handleAdminUITiers(w http.ResponseWriter, r *http.Request) {
	tiers := s.userManager.AllTiers()

	tierList := make([]tierRowData, 0, len(tiers))
	for _, t := range tiers {
		userCount := 0
		for _, u := range s.userManager.AllUsers() {
			if u.Tier == t.Name {
				userCount++
			}
		}
		tierList = append(tierList, tierRowData{
			Name:              t.Name,
			MaxToolIterations: t.MaxToolIterations,
			MaxContextTokens:  t.MaxContextTokens,
			RateLimitPerHour:  t.RateLimitPerHour,
			RateLimitPerDay:   t.RateLimitPerDay,
			Model:             t.Model,
			UserCount:         userCount,
			AllowedTools:      t.AllowedTools,
			DisableTools:      t.DisableTools,
		})
	}

	data := adminPageData{
		Title:   "Tiers",
		Active:  "tiers",
		BaseURL: s.adminBaseURL(r),
		Tiers:   tierList,
	}
	s.renderAdmin(w, "tiers", data)
}

func (s *Server) handleAdminUITierEdit(w http.ResponseWriter, r *http.Request) {
	tierName := strings.TrimPrefix(r.URL.Path, "/admin/tiers/")
	if tierName == "" {
		http.Redirect(w, r, "/admin/tiers", http.StatusSeeOther)
		return
	}

	t := s.userManager.GetTier(tierName)
	if t == nil {
		s.renderAdminError(w, "Tier not found: "+tierName)
		return
	}

	data := adminPageData{
		Title:  "Edit Tier",
		Active: "tiers",
		BaseURL: s.adminBaseURL(r),
		EditTier: &tierRowData{
			Name:              t.Name,
			MaxToolIterations:  t.MaxToolIterations,
			MaxContextTokens:   t.MaxContextTokens,
			RateLimitPerHour:   t.RateLimitPerHour,
			RateLimitPerDay:    t.RateLimitPerDay,
			MaxConcurrentTurns: t.MaxConcurrentTurns,
			MaxWorkspaceBytes:  t.MaxWorkspaceBytes,
			Model:              t.Model,
			AllowedTools:       t.AllowedTools,
			DisableTools:       t.DisableTools,
		},
	}
	s.renderAdmin(w, "tier_edit", data)
}

// ─── MCP Servers ──────────────────────────────────────────────────────────────

func (s *Server) handleAdminUIMCP(w http.ResponseWriter, r *http.Request) {
	var servers []tenant.MCPServerDef
	if s.store != nil {
		servers, _ = s.store.LoadAllMCPServers()
	}

	serverList := make([]mcpRowData, 0, len(servers))
	for _, srv := range servers {
		serverList = append(serverList, mcpRowData{
			ID:      srv.ID,
			UserID:  srv.UserID,
			Name:    srv.Name,
			Command: srv.Command,
			Args:    srv.Args,
			URL:     srv.URL,
			HasEnv:  len(srv.Env) > 0,
		})
	}

	users := s.userManager.AllUsers()
	userIDs := make([]string, 0, len(users))
	for _, u := range users {
		userIDs = append(userIDs, u.ID)
	}

	data := adminPageData{
		Title:     "MCP Servers",
		Active:    "mcp",
		BaseURL:   s.adminBaseURL(r),
		MCPServers: serverList,
		UserIDs:    userIDs,
	}
	s.renderAdmin(w, "mcp", data)
}

// ─── Admin Action Handlers (form POST) ────────────────────────────────────────

func (s *Server) handleAdminActionUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}

	action := r.FormValue("action")
	switch action {
	case "create", "update":
		cfg := tenant.UserConfig{
			ID:                r.FormValue("id"),
			DisplayName:       r.FormValue("displayName"),
			Tier:              r.FormValue("tier"),
			Token:             r.FormValue("token"),
			WorkspaceOverride: r.FormValue("workspace"),
			Admin:             r.FormValue("admin") == "on",
		}

		// Parse channels from textarea (one per line: channel:id)
		channelsText := r.FormValue("channels")
		if channelsText != "" {
			cfg.Channels = parseChannels(channelsText)
		}

		if cfg.ID == "" {
			s.renderAdminError(w, "User ID is required")
			return
		}
		if cfg.Tier == "" {
			cfg.Tier = "default"
		}
		if s.userManager.GetTier(cfg.Tier) == nil {
			s.renderAdminError(w, "Unknown tier: "+cfg.Tier)
			return
		}

		if s.store != nil {
			if err := s.store.SaveUser(cfg); err != nil {
				s.renderAdminError(w, "Failed to save user: "+err.Error())
				return
			}
			actor := userIDFromRequest(r)
			s.store.RecordAdminAction("user."+action, actor, cfg.ID, "tier="+cfg.Tier)
		}
		token, err := s.userManager.RegisterUser(cfg)
		if err != nil {
			s.renderAdminError(w, "Failed to register user: "+err.Error())
			return
		}

		if action == "create" && token != "" {
			http.Redirect(w, r, "/admin/users?created="+cfg.ID+"&token="+token, http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		}

	case "delete":
		userID := r.FormValue("id")
		actor := userIDFromRequest(r)
		if userID == actor {
			s.renderAdminError(w, "Cannot delete your own account")
			return
		}
		if s.store != nil {
			s.store.DeleteUser(userID)
			s.store.RecordAdminAction("user.delete", actor, userID, "")
		}
		s.userManager.RemoveUser(userID)
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)

	default:
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
	}
}

func (s *Server) handleAdminActionTier(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/tiers", http.StatusSeeOther)
		return
	}

	action := r.FormValue("action")
	switch action {
	case "create", "update":
		tc := config.TierConfig{
			Name:              r.FormValue("name"),
			MaxToolIterations: atoiSafe(r.FormValue("maxToolIterations")),
			MaxContextTokens:  atoiSafe(r.FormValue("maxContextTokens")),
			RateLimitPerHour:  atoiSafe(r.FormValue("rateLimitPerHour")),
			RateLimitPerDay:   atoiSafe(r.FormValue("rateLimitPerDay")),
			Model:             r.FormValue("model"),
		}

		if tc.Name == "" {
			s.renderAdminError(w, "Tier name is required")
			return
		}

		// Parse tools from comma-separated text
		if tools := r.FormValue("allowedTools"); tools != "" {
			tc.AllowedTools = splitComma(tools)
		}
		if tools := r.FormValue("disableTools"); tools != "" {
			tc.DisableTools = splitComma(tools)
		}

		if s.store != nil {
			if err := s.store.SaveTier(tc); err != nil {
				s.renderAdminError(w, "Failed to save tier: "+err.Error())
				return
			}
			actor := userIDFromRequest(r)
			s.store.RecordAdminAction("tier."+action, actor, tc.Name, "")
		}
		s.userManager.RegisterTier(tierConfigToTier(tc))
		http.Redirect(w, r, "/admin/tiers", http.StatusSeeOther)

	case "delete":
		name := r.FormValue("name")
		if name == "default" || name == "admin" {
			s.renderAdminError(w, "Cannot delete built-in tier")
			return
		}
		if s.store != nil {
			s.store.DeleteTier(name)
			actor := userIDFromRequest(r)
			s.store.RecordAdminAction("tier.delete", actor, name, "")
		}
		http.Redirect(w, r, "/admin/tiers", http.StatusSeeOther)

	default:
		http.Redirect(w, r, "/admin/tiers", http.StatusSeeOther)
	}
}

func (s *Server) handleAdminActionMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/mcp", http.StatusSeeOther)
		return
	}

	action := r.FormValue("action")
	switch action {
	case "create":
		srv := tenant.MCPServerDef{
			UserID:  r.FormValue("userId"),
			Name:    r.FormValue("name"),
			Command: r.FormValue("command"),
			URL:     r.FormValue("url"),
		}
		if args := r.FormValue("args"); args != "" {
			srv.Args = splitComma(args)
		}
		if env := r.FormValue("env"); env != "" {
			srv.Env = parseEnvVars(env)
		}
		if headers := r.FormValue("headers"); headers != "" {
			srv.Headers = parseEnvVars(headers) // same format key=value
		}

		if srv.Name == "" {
			s.renderAdminError(w, "Name is required")
			return
		}
		if srv.Command == "" && srv.URL == "" {
			s.renderAdminError(w, "Either command or URL is required")
			return
		}

		if s.store != nil {
			if err := s.store.SaveMCPServer(srv); err != nil {
				s.renderAdminError(w, "Failed to save: "+err.Error())
				return
			}
			actor := userIDFromRequest(r)
			s.store.RecordAdminAction("mcp.create", actor, srv.Name, "user="+srv.UserID)
		}
		http.Redirect(w, r, "/admin/mcp", http.StatusSeeOther)

	case "delete":
		name := r.FormValue("name")
		scopeUser := r.FormValue("userId")
		if s.store != nil {
			s.store.DeleteMCPServer(scopeUser, name)
			actor := userIDFromRequest(r)
			s.store.RecordAdminAction("mcp.delete", actor, name, "user="+scopeUser)
		}
		http.Redirect(w, r, "/admin/mcp", http.StatusSeeOther)

	default:
		http.Redirect(w, r, "/admin/mcp", http.StatusSeeOther)
	}
}

// ─── Provider Management UI ───────────────────────────────────────────────────

func (s *Server) handleAdminUIProviders(w http.ResponseWriter, r *http.Request) {
	if s.providerMgr == nil {
		s.renderAdminError(w, "Provider management not enabled")
		return
	}

	defs := s.providerMgr.ListProviders()
	providers := make([]providerRowData, 0, len(defs))
	for _, d := range defs {
		row := providerRowData{
			Name:          d.Name,
			APIBase:       d.APIBase,
			IsPrimary:     d.IsPrimary,
			HasAPIKey:     d.APIKey != "",
			MaxTokens:     d.MaxTokens,
			MaxRetries:    d.MaxRetries,
			TimeoutS:      d.TimeoutS,
			IsFallback:    d.Fallback,
			FallbackOrder: d.FallbackOrder,
			RecoverAfter:  d.RecoverAfter,
		}
		for _, m := range d.Models {
			row.Models = append(row.Models, providerModelRow{
				Name:    m.Name,
				Label:   m.Label,
				Vision:  m.Vision,
				Default: m.Default,
			})
		}
		providers = append(providers, row)
	}

	data := adminPageData{
		Title:     "Providers",
		Active:    "providers",
		BaseURL:   s.adminBaseURL(r),
		Providers: providers,
	}
	s.renderAdmin(w, "providers", data)
}

func (s *Server) handleAdminUIProviderEdit(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/providers/")
	if name == "" {
		http.Redirect(w, r, "/admin/providers", http.StatusSeeOther)
		return
	}

	if s.providerMgr == nil {
		s.renderAdminError(w, "Provider management not enabled")
		return
	}

	// Find provider
	def, found := s.providerMgr.GetProviderDef(name)
	if !found {
		// Maybe it's a new provider — show empty form with name pre-filled
		data := adminPageData{
			Title:        "Add Provider",
			Active:       "providers",
			BaseURL:      s.adminBaseURL(r),
			EditProvider: &providerRowData{Name: name},
		}
		s.renderAdmin(w, "provider_edit", data)
		return
	}

	row := providerRowData{
		Name:          def.Name,
		APIBase:       def.APIBase,
		IsPrimary:     def.IsPrimary,
		HasAPIKey:     def.APIKey != "",
		MaxTokens:     def.MaxTokens,
		MaxRetries:    def.MaxRetries,
		TimeoutS:      def.TimeoutS,
		IsFallback:    def.Fallback,
		FallbackOrder: def.FallbackOrder,
		RecoverAfter:  def.RecoverAfter,
	}
	for _, m := range def.Models {
		row.Models = append(row.Models, providerModelRow{
			Name:    m.Name,
			Label:   m.Label,
			Vision:  m.Vision,
			Default: m.Default,
		})
	}

	data := adminPageData{
		Title:        "Edit Provider",
		Active:       "providers",
		BaseURL:      s.adminBaseURL(r),
		EditProvider: &row,
	}
	s.renderAdmin(w, "provider_edit", data)
}

func (s *Server) handleAdminActionProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin/providers", http.StatusSeeOther)
		return
	}
	if s.providerMgr == nil {
		s.renderAdminError(w, "Provider management not enabled")
		return
	}

	action := r.FormValue("action")
	switch action {
	case "create", "update":
		name := r.FormValue("name")
		if name == "" {
			s.renderAdminError(w, "Provider name is required")
			return
		}
		apiBase := r.FormValue("apiBase")
		if apiBase == "" {
			s.renderAdminError(w, "API Base URL is required")
			return
		}

		// If no API key provided and updating, keep existing
		apiKey := r.FormValue("apiKey")
		if apiKey == "" && action == "update" {
			existing, _ := s.providerMgr.GetProviderDef(name)
			apiKey = existing.APIKey
		}

		def := providers.ProviderConfigDef{
			Name:           name,
			APIBase:        apiBase,
			APIKey:         apiKey,
			IsPrimary:      r.FormValue("isPrimary") == "on",
			MaxTokens:      atoiSafe(r.FormValue("maxTokens")),
			MaxRetries:     atoiSafe(r.FormValue("maxRetries")),
			RetryBaseWaitS: atoiSafe(r.FormValue("retryBaseWaitS")),
			TimeoutS:       atoiSafe(r.FormValue("timeoutS")),
			Fallback:       r.FormValue("isFallback") == "on",
			RecoverAfter:   r.FormValue("recoverAfter"),
			FallbackOrder:  atoiSafe(r.FormValue("fallbackOrder")),
		}

		// Parse models from textarea (one per line: name|label|vision|default)
		modelsText := r.FormValue("models")
		if modelsText != "" {
			for _, line := range strings.Split(modelsText, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				parts := strings.Split(line, "|")
				m := providers.ModelDef{Name: strings.TrimSpace(parts[0])}
				if len(parts) > 1 {
					m.Label = strings.TrimSpace(parts[1])
				}
				if len(parts) > 2 && strings.TrimSpace(parts[2]) == "vision" {
					m.Vision = true
				}
				if len(parts) > 3 && strings.TrimSpace(parts[3]) == "default" {
					m.Default = true
				}
				def.Models = append(def.Models, m)
			}
		}

		if err := s.providerMgr.SaveProvider(def); err != nil {
			s.renderAdminError(w, "Failed to save provider: "+err.Error())
			return
		}

		// Hot-swap if primary
		if def.IsPrimary {
			for _, m := range def.Models {
				if m.Default && s.agentLoop != nil {
					s.agentLoop.UpdateModel(m.Name)
					break
				}
			}
		}

		if s.store != nil {
			actor := userIDFromRequest(r)
			s.store.RecordAdminAction("provider."+action, actor, name, "")
		}
		http.Redirect(w, r, "/admin/providers", http.StatusSeeOther)

	case "delete":
		name := r.FormValue("name")
		if name == "" {
			s.renderAdminError(w, "Provider name required")
			return
		}
		if err := s.providerMgr.DeleteProvider(name); err != nil {
			s.renderAdminError(w, "Failed to delete: "+err.Error())
			return
		}
		if s.store != nil {
			actor := userIDFromRequest(r)
			s.store.RecordAdminAction("provider.delete", actor, name, "")
		}
		http.Redirect(w, r, "/admin/providers", http.StatusSeeOther)

	default:
		http.Redirect(w, r, "/admin/providers", http.StatusSeeOther)
	}
}

// ─── Rendering ────────────────────────────────────────────────────────────────

type adminPageData struct {
	Title         string
	Active        string
	BaseURL       string
	ErrorMessage  string
	SuccessMsg    string
	CurrentTime   time.Time
	Dashboard     *dashboardData
	Users         []userRowData
	Tiers         []tierRowData
	MCPServers    []mcpRowData
	Providers     []providerRowData
	TierNames     []string
	UserIDs       []string
	EditUser      *userRowData
	EditTier      *tierRowData
	EditProvider  *providerRowData
}

type dashboardData struct {
	Users        int
	Tiers        int
	Admins       int
	ActiveNow    int
	MCPServers   int
	Providers    int
	TokenSummary json.RawMessage
}

type userRowData struct {
	ID          string
	DisplayName string
	Tier        string
	Admin       bool
	Channels    map[string]string
	Workspace   string
	CreatedAt   time.Time
}

type tierRowData struct {
	Name               string
	MaxToolIterations  int
	MaxContextTokens   int
	RateLimitPerHour   int
	RateLimitPerDay    int
	MaxConcurrentTurns int
	MaxWorkspaceBytes  int64
	Model              string
	UserCount          int
	AllowedTools       []string
	DisableTools       []string
}

type mcpRowData struct {
	ID      int64
	UserID  string
	Name    string
	Command string
	Args    []string
	URL     string
	HasEnv  bool
}

type providerRowData struct {
	Name        string
	APIBase     string
	IsPrimary   bool
	HasAPIKey   bool
	MaxTokens   int
	MaxRetries  int
	TimeoutS    int
	IsFallback  bool
	FallbackOrder int
	RecoverAfter string
	Models      []providerModelRow
}

type providerModelRow struct {
	Name    string
	Label   string
	Vision  bool
	Default bool
}

func (s *Server) renderAdmin(w http.ResponseWriter, name string, data adminPageData) {
	tmpl := getAdminTemplates()
	if tmpl == nil {
		http.Error(w, "Admin templates not available", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) renderAdminError(w http.ResponseWriter, msg string) {
	data := adminPageData{
		Title:        "Error",
		Active:       "",
		ErrorMessage: msg,
	}
	s.renderAdmin(w, "error", data)
}

func (s *Server) adminBaseURL(r *http.Request) string {
	return ""
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func countAdmins(users []tenant.UserConfig) int {
	count := 0
	for _, u := range users {
		if u.Admin {
			count++
		}
	}
	return count
}

func countActive(users []tenant.UserConfig, duration time.Duration) int {
	count := 0
	cutoff := time.Now().Add(-duration)
	for _, u := range users {
		if u.CreatedAt.After(cutoff) {
			count++
		}
	}
	return count
}

func mapError(code string) string {
	switch code {
	case "missing_token":
		return "Please enter your admin token."
	case "invalid_token":
		return "Invalid token or not an admin account."
	case "not_admin":
		return "This account does not have admin privileges."
	default:
		return ""
	}
}

func parseChannels(text string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			result[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return result
}

func parseEnvVars(text string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			result[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return result
}

func splitComma(s string) []string {
	var result []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func atoiSafe(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}
