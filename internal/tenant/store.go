package tenant

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/wltechblog/gino/internal/config"

	_ "modernc.org/sqlite"
)

// Store provides persistent storage for tenant configuration.
// It stores users, tiers, and per-user MCP server definitions in SQLite
// so that provisioning changes survive restarts.
type Store struct {
	mu sync.Mutex
	db *sql.DB
}

// OpenStore opens or creates the tenant store at the given path.
func OpenStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("tenant store: open: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite limitation

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("tenant store: migrate: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS tenant_users (
			id           TEXT PRIMARY KEY,
			display_name TEXT NOT NULL DEFAULT '',
			tier         TEXT NOT NULL DEFAULT 'default',
			token        TEXT NOT NULL DEFAULT '',
			channels     TEXT NOT NULL DEFAULT '{}',
			workspace    TEXT NOT NULL DEFAULT '',
			admin        INTEGER NOT NULL DEFAULT 0,
			created_at   TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS tenant_tiers (
			name              TEXT PRIMARY KEY,
			config            TEXT NOT NULL DEFAULT '{}',
			updated_at        TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS tenant_mcp_servers (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id   TEXT NOT NULL DEFAULT '',   -- empty = global server
			name      TEXT NOT NULL,
			command   TEXT NOT NULL DEFAULT '',
			args      TEXT NOT NULL DEFAULT '[]',  -- JSON array
			url       TEXT NOT NULL DEFAULT '',
			headers   TEXT NOT NULL DEFAULT '{}',  -- JSON object
			env       TEXT NOT NULL DEFAULT '{}',  -- JSON object
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(user_id, name)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_mcp_user ON tenant_mcp_servers(user_id)`,
		`CREATE TABLE IF NOT EXISTS tenant_audit (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			event     TEXT NOT NULL,
			actor     TEXT NOT NULL DEFAULT '',
			target    TEXT NOT NULL DEFAULT '',
			detail    TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
	}
	
	// Also migrate provider tables in the same DB
	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("exec migration: %w", err)
		}
	}
	
	// Provider tables (shared in the same SQLite DB)
	providerMigrations := []string{
		`CREATE TABLE IF NOT EXISTS providers (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			name         TEXT NOT NULL UNIQUE,
			api_base     TEXT NOT NULL DEFAULT '',
			api_key      TEXT NOT NULL DEFAULT '',
			is_primary   INTEGER NOT NULL DEFAULT 0,
			max_tokens   INTEGER NOT NULL DEFAULT 0,
			max_retries  INTEGER NOT NULL DEFAULT 0,
			retry_base_wait_s INTEGER NOT NULL DEFAULT 0,
			timeout_s    INTEGER NOT NULL DEFAULT 0,
			is_fallback  INTEGER NOT NULL DEFAULT 0,
			recover_after TEXT NOT NULL DEFAULT '',
			fallback_order INTEGER NOT NULL DEFAULT 0,
			updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS provider_models (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			provider_id  INTEGER NOT NULL,
			name         TEXT NOT NULL,
			label        TEXT NOT NULL DEFAULT '',
			vision       INTEGER NOT NULL DEFAULT 0,
			is_default   INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY(provider_id) REFERENCES providers(id) ON DELETE CASCADE,
			UNIQUE(provider_id, name)
		)`,
	}
	for _, m := range providerMigrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("exec provider migration: %w", err)
		}
	}
	return nil
}

// DB returns the underlying *sql.DB for use by other managers
// (e.g. ProviderManager) that share the same SQLite database.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// ─── Users ─────────────────────────────────────────────────────────────────────

// SaveUser inserts or updates a user record.
func (s *Store) SaveUser(u UserConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	channels, _ := json.Marshal(u.Channels)
	_, err := s.db.Exec(`INSERT INTO tenant_users (id, display_name, tier, token, channels, workspace, admin, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
			display_name = excluded.display_name,
			tier = excluded.tier,
			token = excluded.token,
			channels = excluded.channels,
			workspace = excluded.workspace,
			admin = excluded.admin,
			updated_at = datetime('now')`,
		u.ID, u.DisplayName, u.Tier, u.Token, string(channels), u.WorkspaceOverride, boolToInt(u.Admin))
	return err
}

// DeleteUser removes a user record.
func (s *Store) DeleteUser(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM tenant_users WHERE id = ?`, id)
	return err
}

// LoadUsers returns all persisted users.
func (s *Store) LoadUsers() ([]UserConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT id, display_name, tier, token, channels, workspace, admin FROM tenant_users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []UserConfig
	for rows.Next() {
		var u UserConfig
		var channelsJSON string
		var adminInt int
		if err := rows.Scan(&u.ID, &u.DisplayName, &u.Tier, &u.Token, &channelsJSON, &u.WorkspaceOverride, &adminInt); err != nil {
			return nil, err
		}
		u.Admin = adminInt != 0
		_ = json.Unmarshal([]byte(channelsJSON), &u.Channels)
		users = append(users, u)
	}
	return users, rows.Err()
}

// ─── Tiers ────────────────────────────────────────────────────────────────────

// SaveTier inserts or updates a tier definition.
func (s *Store) SaveTier(t config.TierConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO tenant_tiers (name, config, updated_at)
		VALUES (?, ?, datetime('now'))
		ON CONFLICT(name) DO UPDATE SET config = excluded.config, updated_at = datetime('now')`,
		t.Name, string(cfg))
	return err
}

// LoadTiers returns all persisted tiers.
func (s *Store) LoadTiers() ([]config.TierConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT config FROM tenant_tiers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tiers []config.TierConfig
	for rows.Next() {
		var cfgJSON string
		if err := rows.Scan(&cfgJSON); err != nil {
			return nil, err
		}
		var t config.TierConfig
		if err := json.Unmarshal([]byte(cfgJSON), &t); err != nil {
			continue
		}
		tiers = append(tiers, t)
	}
	return tiers, rows.Err()
}

// DeleteTier removes a tier definition from the store.
func (s *Store) DeleteTier(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM tenant_tiers WHERE name = ?`, name)
	return err
}

// ─── MCP Servers ───────────────────────────────────────────────────────────────

// MCPServerDef is a persisted MCP server configuration.
// It mirrors config.MCPServerConfig but is stored per-user (or globally).
type MCPServerDef struct {
	ID       int64  `json:"id,omitempty"`
	UserID   string `json:"userId,omitempty"` // empty = global
	Name     string `json:"name"`
	Command  string `json:"command,omitempty"`
	Args     []string `json:"args,omitempty"`
	URL      string `json:"url,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

// SaveMCPServer inserts or updates an MCP server definition.
func (s *Store) SaveMCPServer(srv MCPServerDef) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	args, _ := json.Marshal(srv.Args)
	headers, _ := json.Marshal(srv.Headers)
	env, _ := json.Marshal(srv.Env)

	if srv.ID > 0 {
		_, err := s.db.Exec(`UPDATE tenant_mcp_servers SET command=?, args=?, url=?, headers=?, env=? WHERE id=?`,
			srv.Command, string(args), srv.URL, string(headers), string(env), srv.ID)
		return err
	}

	_, err := s.db.Exec(`INSERT INTO tenant_mcp_servers (user_id, name, command, args, url, headers, env)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, name) DO UPDATE SET
			command = excluded.command,
			args = excluded.args,
			url = excluded.url,
			headers = excluded.headers,
			env = excluded.env`,
		srv.UserID, srv.Name, srv.Command, string(args), srv.URL, string(headers), string(env))
	return err
}

// DeleteMCPServer removes an MCP server by user + name.
func (s *Store) DeleteMCPServer(userID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM tenant_mcp_servers WHERE user_id = ? AND name = ?`, userID, name)
	return err
}

// LoadMCPServers returns MCP servers for a specific user (plus globals).
func (s *Store) LoadMCPServers(userID string) ([]MCPServerDef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT id, user_id, name, command, args, url, headers, env
		FROM tenant_mcp_servers WHERE user_id = '' OR user_id = ?
		ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var servers []MCPServerDef
	for rows.Next() {
		var srv MCPServerDef
		var argsJSON, headersJSON, envJSON string
		if err := rows.Scan(&srv.ID, &srv.UserID, &srv.Name, &srv.Command, &argsJSON, &srv.URL, &headersJSON, &envJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(argsJSON), &srv.Args)
		_ = json.Unmarshal([]byte(headersJSON), &srv.Headers)
		_ = json.Unmarshal([]byte(envJSON), &srv.Env)
		servers = append(servers, srv)
	}
	return servers, rows.Err()
}

// LoadAllMCPServers returns all MCP servers (all users + globals).
func (s *Store) LoadAllMCPServers() ([]MCPServerDef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT id, user_id, name, command, args, url, headers, env FROM tenant_mcp_servers ORDER BY user_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var servers []MCPServerDef
	for rows.Next() {
		var srv MCPServerDef
		var argsJSON, headersJSON, envJSON string
		if err := rows.Scan(&srv.ID, &srv.UserID, &srv.Name, &srv.Command, &argsJSON, &srv.URL, &headersJSON, &envJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(argsJSON), &srv.Args)
		_ = json.Unmarshal([]byte(headersJSON), &srv.Headers)
		_ = json.Unmarshal([]byte(envJSON), &srv.Env)
		servers = append(servers, srv)
	}
	return servers, rows.Err()
}

// ─── Audit Log ─────────────────────────────────────────────────────────────────

// RecordAdminAction logs an admin operation for accountability.
func (s *Store) RecordAdminAction(event, actor, target, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO tenant_audit (event, actor, target, detail) VALUES (?, ?, ?, ?)`,
		event, actor, target, detail)
	if err != nil {
		log.Printf("tenant store: failed to record audit: %v", err)
	}
}

// ─── Helpers ───────────────────────────────────────────────────────────────────

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}


