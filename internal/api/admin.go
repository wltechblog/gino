package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
	"github.com/wltechblog/gino/internal/tenant"
)

// ============================================================================
// Admin API — User, Tier, and MCP Server Management
// ============================================================================
//
// All admin endpoints require:
//   1. Valid authentication (bearer token)
//   2. Admin privilege (UserConfig.Admin == true)
//
// Changes are persisted to the tenant Store (SQLite) and hot-reloaded into
// the UserManager so they take effect immediately without restart.

// ─── Admin Middleware ──────────────────────────────────────────────────────────

// adminMiddleware wraps a handler requiring admin privilege.
// Must be applied AFTER authMiddleware.
func (s *Server) adminMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.userManager == nil {
			s.writeError(w, http.StatusServiceUnavailable, "Multi-tenant mode not enabled")
			return
		}

		userID := userIDFromRequest(r)

		// Check admin privilege
		uctx := s.userManager.Get(userID)
		if uctx == nil || !uctx.IsAdmin() {
			s.writeError(w, http.StatusForbidden, "Admin access required")
			return
		}

		next(w, r)
	}
}

// ─── Users ─────────────────────────────────────────────────────────────────────

// UserCreateRequest is the payload for creating/updating a user.
type UserCreateRequest struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"displayName,omitempty"`
	Tier        string            `json:"tier"`
	Token       string            `json:"token,omitempty"`
	Channels    map[string]string `json:"channels,omitempty"`
	Workspace   string            `json:"workspace,omitempty"`
	Admin       bool              `json:"admin,omitempty"`
}

// UserResponse is the user record returned by the API.
type UserResponse struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"displayName,omitempty"`
	Tier        string            `json:"tier"`
	Token       string            `json:"token,omitempty"` // only returned on create
	Channels    map[string]string `json:"channels,omitempty"`
	Admin       bool              `json:"admin,omitempty"`
	Workspace   string            `json:"workspace,omitempty"`
	CreatedAt   string            `json:"createdAt,omitempty"`
}

// handleAdminUsers handles GET (list) and POST (create) for users.
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.adminListUsers(w, r)
	case http.MethodPost:
		s.adminCreateUser(w, r)
	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Use GET or POST")
	}
}

// handleAdminUser handles GET, PUT, DELETE for a single user.
func (s *Server) handleAdminUser(w http.ResponseWriter, r *http.Request) {
	userID := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/users/")
	if userID == "" {
		s.writeError(w, http.StatusBadRequest, "User ID required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.adminGetUser(w, r, userID)
	case http.MethodPut:
		s.adminUpdateUser(w, r, userID)
	case http.MethodDelete:
		s.adminDeleteUser(w, r, userID)
	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Use GET, PUT, or DELETE")
	}
}

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	users := s.userManager.AllUsers()
	resp := make([]UserResponse, 0, len(users))
	for _, u := range users {
		resp = append(resp, userConfigToResponse(u))
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"users": resp,
		"total": len(resp),
	})
}

func (s *Server) adminGetUser(w http.ResponseWriter, r *http.Request, userID string) {
	uctx := s.userManager.Get(userID)
	if uctx == nil {
		s.writeError(w, http.StatusNotFound, "User not found")
		return
	}
	s.writeJSON(w, http.StatusOK, userConfigToResponse(uctx.Config))
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var req UserCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.ID == "" {
		s.writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if req.Tier == "" {
		req.Tier = "default"
	}

	// Check tier exists
	if s.userManager.GetTier(req.Tier) == nil {
		s.writeError(w, http.StatusBadRequest, "Unknown tier: "+req.Tier)
		return
	}

	cfg := tenant.UserConfig{
		ID:                req.ID,
		DisplayName:       req.DisplayName,
		Tier:              req.Tier,
		Token:             req.Token,
		Channels:          req.Channels,
		WorkspaceOverride: req.Workspace,
		Admin:             req.Admin,
	}

	// Persist
	if s.store != nil {
		if err := s.store.SaveUser(cfg); err != nil {
			s.writeError(w, http.StatusInternalServerError, "Failed to persist user: "+err.Error())
			return
		}
		actor := userIDFromRequest(r)
		s.store.RecordAdminAction("user.create", actor, req.ID, "tier="+req.Tier)
	}

	// Hot-reload into UserManager
	token, err := s.userManager.RegisterUser(cfg)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to register user: "+err.Error())
		return
	}

	resp := userConfigToResponse(cfg)
	resp.Token = token
	s.writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) adminUpdateUser(w http.ResponseWriter, r *http.Request, userID string) {
	var req UserCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	// Merge with existing
	existing := s.userManager.Get(userID)
	if existing == nil {
		s.writeError(w, http.StatusNotFound, "User not found")
		return
	}

	cfg := existing.Config
	if req.DisplayName != "" {
		cfg.DisplayName = req.DisplayName
	}
	if req.Tier != "" {
		if s.userManager.GetTier(req.Tier) == nil {
			s.writeError(w, http.StatusBadRequest, "Unknown tier: "+req.Tier)
			return
		}
		cfg.Tier = req.Tier
	}
	if req.Token != "" {
		cfg.Token = req.Token
	}
	if req.Channels != nil {
		cfg.Channels = req.Channels
	}
	if req.Workspace != "" {
		cfg.WorkspaceOverride = req.Workspace
	}
	cfg.Admin = req.Admin // always set, default false

	// Persist
	if s.store != nil {
		if err := s.store.SaveUser(cfg); err != nil {
			s.writeError(w, http.StatusInternalServerError, "Failed to persist: "+err.Error())
			return
		}
		actor := userIDFromRequest(r)
		s.store.RecordAdminAction("user.update", actor, userID, "tier="+cfg.Tier)
	}

	// Hot-reload
	if _, err := s.userManager.RegisterUser(cfg); err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to update user: "+err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, userConfigToResponse(cfg))
}

func (s *Server) adminDeleteUser(w http.ResponseWriter, r *http.Request, userID string) {
	// Prevent self-deletion
	actor := userIDFromRequest(r)
	if userID == actor {
		s.writeError(w, http.StatusBadRequest, "Cannot delete your own account")
		return
	}

	// Persist
	if s.store != nil {
		if err := s.store.DeleteUser(userID); err != nil {
			s.writeError(w, http.StatusInternalServerError, "Failed to delete: "+err.Error())
			return
		}
		s.store.RecordAdminAction("user.delete", actor, userID, "")
	}

	// Hot-remove from UserManager
	s.userManager.RemoveUser(userID)

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ─── Tiers ─────────────────────────────────────────────────────────────────────

// TierResponse is the tier definition returned by the API.
type TierResponse struct {
	Name               string            `json:"name"`
	MaxToolIterations  int               `json:"maxToolIterations,omitempty"`
	MaxContextTokens   int               `json:"maxContextTokens,omitempty"`
	AllowedTools       []string          `json:"allowedTools,omitempty"`
	DisableTools       []string          `json:"disableTools,omitempty"`
	RateLimitPerHour   int               `json:"rateLimitPerHour,omitempty"`
	RateLimitPerDay    int               `json:"rateLimitPerDay,omitempty"`
	Model              string            `json:"model,omitempty"`
	Models             []config.ModelOption `json:"models,omitempty"`
	AllowedMCP         []string          `json:"allowedMcp,omitempty"`
}

func (s *Server) handleAdminTiers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.adminListTiers(w, r)
	case http.MethodPost:
		s.adminCreateTier(w, r)
	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Use GET or POST")
	}
}

func (s *Server) adminListTiers(w http.ResponseWriter, r *http.Request) {
	tiers := s.userManager.AllTiers()
	resp := make([]TierResponse, 0, len(tiers))
	for _, t := range tiers {
		resp = append(resp, tierToResponse(t))
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"tiers": resp,
	})
}

func (s *Server) adminCreateTier(w http.ResponseWriter, r *http.Request) {
	var req config.TierConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.Name == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Persist
	if s.store != nil {
		if err := s.store.SaveTier(req); err != nil {
			s.writeError(w, http.StatusInternalServerError, "Failed to persist tier: "+err.Error())
			return
		}
		actor := userIDFromRequest(r)
		s.store.RecordAdminAction("tier.create", actor, req.Name, "")
	}

	// Hot-reload into UserManager
	tier := tierConfigToTier(req)
	s.userManager.RegisterTier(tier)

	s.writeJSON(w, http.StatusCreated, tierToResponse(req))
}

// ─── MCP Servers ───────────────────────────────────────────────────────────────

// MCPServerRequest is the payload for creating/updating an MCP server.
type MCPServerRequest struct {
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// MCPServerResponse is the MCP server record returned by the API.
type MCPServerResponse struct {
	ID      int64             `json:"id,omitempty"`
	UserID  string            `json:"userId,omitempty"`
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	HasEnv  bool              `json:"hasEnv"` // don't expose env values
}

// handleAdminMCPServers handles GET (list) and POST (create) for MCP servers.
// Supports optional /api/v1/admin/mcp/{userID} to scope to a user.
func (s *Server) handleAdminMCPServers(w http.ResponseWriter, r *http.Request) {
	// Extract optional userID from path
	pathUser := ""
	if strings.HasPrefix(r.URL.Path, "/api/v1/admin/mcp/") {
		pathUser = strings.TrimPrefix(r.URL.Path, "/api/v1/admin/mcp/")
		// If there's nothing after /mcp/, treat as global list
		if pathUser == "" || pathUser == "/" {
			pathUser = ""
		}
	}

	switch r.Method {
	case http.MethodGet:
		s.adminListMCPServers(w, r, pathUser)
	case http.MethodPost:
		s.adminCreateMCPServer(w, r, pathUser)
	case http.MethodDelete:
		s.adminDeleteMCPServer(w, r, pathUser)
	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Use GET, POST, or DELETE")
	}
}

func (s *Server) adminListMCPServers(w http.ResponseWriter, r *http.Request, scopeUser string) {
	if s.store == nil {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"servers": []MCPServerResponse{}})
		return
	}

	var servers []tenant.MCPServerDef
	var err error
	if scopeUser != "" {
		servers, err = s.store.LoadMCPServers(scopeUser)
	} else {
		servers, err = s.store.LoadAllMCPServers()
	}
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to load MCP servers: "+err.Error())
		return
	}

	resp := make([]MCPServerResponse, 0, len(servers))
	for _, srv := range servers {
		resp = append(resp, mcpServerToResponse(srv))
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"servers": resp,
		"total":   len(resp),
	})
}

func (s *Server) adminCreateMCPServer(w http.ResponseWriter, r *http.Request, scopeUser string) {
	var req MCPServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.Name == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Command == "" && req.URL == "" {
		s.writeError(w, http.StatusBadRequest, "Either command or url is required")
		return
	}

	srv := tenant.MCPServerDef{
		UserID:  scopeUser,
		Name:    req.Name,
		Command: req.Command,
		Args:    req.Args,
		URL:     req.URL,
		Headers: req.Headers,
		Env:     req.Env,
	}

	if s.store != nil {
		if err := s.store.SaveMCPServer(srv); err != nil {
			s.writeError(w, http.StatusInternalServerError, "Failed to persist MCP server: "+err.Error())
			return
		}
		actor := userIDFromRequest(r)
		s.store.RecordAdminAction("mcp.create", actor, req.Name, "user="+scopeUser)
	}

	s.writeJSON(w, http.StatusCreated, mcpServerToResponse(srv))
}

func (s *Server) adminDeleteMCPServer(w http.ResponseWriter, r *http.Request, scopeUser string) {
	name := r.URL.Query().Get("name")
	if name == "" {
		s.writeError(w, http.StatusBadRequest, "name query parameter required")
		return
	}

	if s.store != nil {
		if err := s.store.DeleteMCPServer(scopeUser, name); err != nil {
			s.writeError(w, http.StatusInternalServerError, "Failed to delete: "+err.Error())
			return
		}
		actor := userIDFromRequest(r)
		s.store.RecordAdminAction("mcp.delete", actor, name, "user="+scopeUser)
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ─── Providers ─────────────────────────────────────────────────────────────────

// ProviderRequest is the payload for creating/updating a provider.
type ProviderRequest struct {
	Name           string             `json:"name"`
	APIBase        string             `json:"apiBase"`
	APIKey         string             `json:"apiKey"`
	IsPrimary      bool               `json:"isPrimary"`
	MaxTokens      int                `json:"maxTokens,omitempty"`
	MaxRetries     int                `json:"maxRetries,omitempty"`
	RetryBaseWaitS int                `json:"retryBaseWaitS,omitempty"`
	TimeoutS       int                `json:"timeoutS,omitempty"`
	Fallback       bool               `json:"fallback,omitempty"`
	RecoverAfter   string             `json:"recoverAfter,omitempty"`
	FallbackOrder  int                `json:"fallbackOrder,omitempty"`
	Models         []providers.ModelDef `json:"models,omitempty"`
}

// handleAdminProviders handles GET (list) and POST (create/update) for providers.
func (s *Server) handleAdminProviders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.adminListProviders(w, r)
	case http.MethodPost:
		s.adminSaveProvider(w, r)
	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Use GET or POST")
	}
}

// handleAdminProvider handles DELETE for a single provider.
func (s *Server) handleAdminProvider(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/providers/")
	if name == "" {
		s.writeError(w, http.StatusBadRequest, "Provider name required")
		return
	}

	switch r.Method {
	case http.MethodDelete:
		s.adminDeleteProvider(w, r, name)
	default:
		s.writeError(w, http.StatusMethodNotAllowed, "Use DELETE")
	}
}

func (s *Server) adminListProviders(w http.ResponseWriter, r *http.Request) {
	if s.providerMgr == nil {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"providers": []interface{}{}})
		return
	}
	defs := s.providerMgr.ListProviders()
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"providers": defs,
		"total":     len(defs),
	})
}

func (s *Server) adminSaveProvider(w http.ResponseWriter, r *http.Request) {
	if s.providerMgr == nil {
		s.writeError(w, http.StatusServiceUnavailable, "Provider management not enabled")
		return
	}

	var req ProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	if req.Name == "" {
		s.writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.APIBase == "" {
		s.writeError(w, http.StatusBadRequest, "apiBase is required")
		return
	}

	// If updating and no APIKey provided, keep existing
	if req.APIKey == "" {
		existing := s.providerMgr.GetProvider(req.Name)
		_ = existing // can't get key back; user must re-enter if changing
	}

	def := providers.ProviderConfigDef{
		Name:           req.Name,
		APIBase:        req.APIBase,
		APIKey:         req.APIKey,
		IsPrimary:      req.IsPrimary,
		MaxTokens:      req.MaxTokens,
		MaxRetries:     req.MaxRetries,
		RetryBaseWaitS: req.RetryBaseWaitS,
		TimeoutS:       req.TimeoutS,
		Fallback:       req.Fallback,
		RecoverAfter:   req.RecoverAfter,
		FallbackOrder:  req.FallbackOrder,
		Models:         req.Models,
	}

	if err := s.providerMgr.SaveProvider(def); err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to save provider: "+err.Error())
		return
	}

	// If this is primary and has a default model, update the agent loop
	if req.IsPrimary {
		for _, m := range req.Models {
			if m.Default && s.agentLoop != nil {
				s.agentLoop.UpdateModel(m.Name)
				break
			}
		}
	}

	actor := userIDFromRequest(r)
	if s.store != nil {
		s.store.RecordAdminAction("provider.save", actor, req.Name, "")
	}

	s.writeJSON(w, http.StatusCreated, map[string]string{"status": "saved"})
}

func (s *Server) adminDeleteProvider(w http.ResponseWriter, r *http.Request, name string) {
	if s.providerMgr == nil {
		s.writeError(w, http.StatusServiceUnavailable, "Provider management not enabled")
		return
	}

	if err := s.providerMgr.DeleteProvider(name); err != nil {
		s.writeError(w, http.StatusInternalServerError, "Failed to delete: "+err.Error())
		return
	}

	actor := userIDFromRequest(r)
	if s.store != nil {
		s.store.RecordAdminAction("provider.delete", actor, name, "")
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ─── Helpers ───────────────────────────────────────────────────────────────────

func userConfigToResponse(u tenant.UserConfig) UserResponse {
	return UserResponse{
		ID:          u.ID,
		DisplayName: u.DisplayName,
		Tier:        u.Tier,
		Channels:    u.Channels,
		Admin:       u.Admin,
		Workspace:   u.WorkspaceOverride,
	}
}

func tierToResponse(t config.TierConfig) TierResponse {
	return TierResponse{
		Name:              t.Name,
		MaxToolIterations: t.MaxToolIterations,
		MaxContextTokens:  t.MaxContextTokens,
		AllowedTools:      t.AllowedTools,
		DisableTools:      t.DisableTools,
		RateLimitPerHour:  t.RateLimitPerHour,
		RateLimitPerDay:   t.RateLimitPerDay,
		Model:             t.Model,
		Models:            t.Models,
		AllowedMCP:        t.AllowedMCP,
	}
}

func tierConfigToTier(tc config.TierConfig) *tenant.Tier {
	return &tenant.Tier{
		Name:              tc.Name,
		MaxToolIterations: tc.MaxToolIterations,
		MaxContextTokens:  tc.MaxContextTokens,
		AllowedTools:      tc.AllowedTools,
		DisableTools:      tc.DisableTools,
		RateLimitPerHour:  tc.RateLimitPerHour,
		RateLimitPerDay:   tc.RateLimitPerDay,
		MaxConcurrentTurns: tc.MaxConcurrentTurns,
		MaxWorkspaceBytes: tc.MaxWorkspaceBytes,
		MaxFileUploadBytes: tc.MaxFileUploadBytes,
		Model:             tc.Model,
		Sandbox:           tc.Sandbox,
		AllowedMCP:        tc.AllowedMCP,
	}
}

func mcpServerToResponse(srv tenant.MCPServerDef) MCPServerResponse {
	return MCPServerResponse{
		ID:      srv.ID,
		UserID:  srv.UserID,
		Name:    srv.Name,
		Command: srv.Command,
		Args:    srv.Args,
		URL:     srv.URL,
		Headers: srv.Headers,
		HasEnv:  len(srv.Env) > 0,
	}
}
