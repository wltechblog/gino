package mcp

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrOAuthRequired is returned when an MCP server requires OAuth authentication.
// The AuthURL should be shown to the user. After they authenticate and are redirected,
// they paste the redirect URL back, which is passed to CompleteAuth.
type ErrOAuthRequired struct {
	ServerName string
	AuthURL    string
	// ServerKey is the URL used to key token storage (the MCP server URL).
	ServerKey string
}

func (e *ErrOAuthRequired) Error() string {
	return fmt.Sprintf("MCP server %q requires OAuth authentication. Open this URL to authenticate: %s", e.ServerName, e.AuthURL)
}

// Token represents an OAuth access token with optional refresh token.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Scope        string    `json:"scope,omitempty"`
}

func (t *Token) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(t.ExpiresAt.Add(-60 * time.Second)) // 60s skew
}

// pendingAuth holds state needed to complete the OAuth flow after the user authenticates.
type pendingAuth struct {
	CodeVerifier         string                 `json:"code_verifier"`
	State                string                 `json:"state"`
	ClientID             string                 `json:"client_id"`
	RedirectURI          string                 `json:"redirect_uri"`
	TokenEndpoint        string                 `json:"token_endpoint"`
	AuthServerMetadataURL string                `json:"-"`
	Metadata             map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt            time.Time              `json:"created_at"`
}

// TokenStore manages OAuth tokens on disk, keyed by server URL.
type TokenStore struct {
	path   string
	mu     sync.Mutex
	tokens map[string]Token
	pending map[string]pendingAuth
	loaded  bool
}

// NewTokenStore creates a token store backed by a JSON file.
func NewTokenStore(homeDir string) *TokenStore {
	return &TokenStore{
		path:    filepath.Join(homeDir, "tokens.json"),
		tokens:  make(map[string]Token),
		pending: make(map[string]pendingAuth),
	}
}

func (ts *TokenStore) load() {
	if ts.loaded {
		return
	}
	ts.loaded = true
	data, err := os.ReadFile(ts.path)
	if err != nil {
		return // file doesn't exist yet — fine
	}
	// Try new format with both tokens and pending
	var combined struct {
		Tokens  map[string]Token       `json:"tokens"`
		Pending map[string]pendingAuth `json:"pending"`
	}
	if json.Unmarshal(data, &combined) == nil {
		if combined.Tokens != nil {
			ts.tokens = combined.Tokens
		}
		if combined.Pending != nil {
			ts.pending = combined.Pending
		}
		return
	}
	// Fallback: old format (just a map of tokens)
	var oldTokens map[string]Token
	if json.Unmarshal(data, &oldTokens) == nil {
		ts.tokens = oldTokens
	}
}

func (ts *TokenStore) save() {
	ts.load()
	combined := struct {
		Tokens  map[string]Token       `json:"tokens"`
		Pending map[string]pendingAuth `json:"pending"`
	}{
		Tokens:  ts.tokens,
		Pending: ts.pending,
	}
	data, err := json.MarshalIndent(combined, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(ts.path, data, 0600)
}

// GetToken returns a cached token for the given server URL, if one exists.
func (ts *TokenStore) GetToken(serverURL string) (Token, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.load()
	t, ok := ts.tokens[serverURL]
	return t, ok
}

// SetToken stores a token for the given server URL.
func (ts *TokenStore) SetToken(serverURL string, token Token) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.load()
	ts.tokens[serverURL] = token
	ts.save()
}

// DeleteToken removes a token (e.g., when refresh fails).
func (ts *TokenStore) DeleteToken(serverURL string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.load()
	delete(ts.tokens, serverURL)
	ts.save()
}

// SetPending stores pending OAuth state for a server URL.
func (ts *TokenStore) SetPending(serverURL string, pa pendingAuth) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.load()
	ts.pending[serverURL] = pa
	ts.save()
}

// GetPending retrieves and removes pending OAuth state.
func (ts *TokenStore) GetPending(serverURL string) (pendingAuth, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.load()
	pa, ok := ts.pending[serverURL]
	return pa, ok
}

// ClearPending removes pending OAuth state.
func (ts *TokenStore) ClearPending(serverURL string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.load()
	delete(ts.pending, serverURL)
	ts.save()
}

/*** PKCE ***/

// generatePKCE creates a code verifier and S256 code challenge.
func generatePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("pkce: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

// generateState creates a random state parameter for CSRF protection.
func generateState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

/*** OAuth Discovery ***/

// protectedResourceMetadata represents the OAuth 2.0 Protected Resource Metadata (RFC9728).
type protectedResourceMetadata struct {
	AuthorizationServers     []string `json:"authorization_servers"`
	Resource                 string   `json:"resource"`
	BearerMethodsSupported   []string `json:"bearer_methods_supported"`
	ScopesSupported          []string `json:"scopes_supported"`
}

// authorizationServerMetadata represents the OAuth 2.0 Authorization Server Metadata (RFC8414).
type authorizationServerMetadata struct {
	AuthorizationEndpoint        string   `json:"authorization_endpoint"`
	TokenEndpoint                string   `json:"token_endpoint"`
	RegistrationEndpoint         string   `json:"registration_endpoint"`
	RevocationEndpoint           string   `json:"revocation_endpoint"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
	GrantTypesSupported          []string  `json:"grant_types_supported"`
	ResponseTypesSupported       []string  `json:"response_types_supported"`
	ScopesSupported              []string  `json:"scopes_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
}

// oauthManager handles the OAuth flow for an MCP HTTP server.
type oauthManager struct {
	serverURL  string
	serverName string
	store      *TokenStore
	httpClient *http.Client
}

func newOAuthManager(serverURL, serverName string, store *TokenStore) *oauthManager {
	return &oauthManager{
		serverURL:  serverURL,
		serverName: serverName,
		store:      store,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// discoverProtectedResource fetches the protected resource metadata using the
// WWW-Authenticate header from a 401 response, or by trying the well-known path.
func (om *oauthManager) discoverProtectedResource(authHeader string) (*protectedResourceMetadata, error) {
	var metadataURL string

	// Parse WWW-Authenticate header per RFC9728 Section 5.1
	// Format: Bearer resource_metadata="https://..."
	if authHeader != "" {
		// Try to extract resource_metadata URL
		if v := parseWWWAuthenticate(authHeader); v != "" {
			metadataURL = v
		}
	}

	// Fallback: construct well-known path from server URL
	if metadataURL == "" {
		parsed, err := url.Parse(om.serverURL)
		if err != nil {
			return nil, fmt.Errorf("parse server URL: %w", err)
		}
		metadataURL = parsed.Scheme + "://" + parsed.Host + "/.well-known/oauth-protected-resource"
	}

	resp, err := om.httpClient.Get(metadataURL)
	if err != nil {
		return nil, fmt.Errorf("fetch resource metadata: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("resource metadata: HTTP %d", resp.StatusCode)
	}

	var prm protectedResourceMetadata
	if err := json.NewDecoder(resp.Body).Decode(&prm); err != nil {
		return nil, fmt.Errorf("decode resource metadata: %w", err)
	}

	if len(prm.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("no authorization_servers in resource metadata")
	}

	return &prm, nil
}

// discoverAuthServer fetches the authorization server metadata.
// Per RFC 8414, the metadata is at <issuer>/.well-known/oauth-authorization-server.
// Some providers (e.g. Robinhood) publish it at the origin root rather than the
// full path, so we try the full URL first and fall back to origin-only.
func (om *oauthManager) discoverAuthServer(authServerURL string) (*authorizationServerMetadata, error) {
	candidates := []string{
		strings.TrimRight(authServerURL, "/") + "/.well-known/oauth-authorization-server",
	}

	// Add origin-only fallback (scheme://host/.well-known/...)
	if parsed, err := url.Parse(authServerURL); err == nil && parsed.Path != "" && parsed.Path != "/" {
		candidates = append(candidates, parsed.Scheme+"://"+parsed.Host+"/.well-known/oauth-authorization-server")
	}

	var lastStatus int
	for _, metadataURL := range candidates {
		resp, err := om.httpClient.Get(metadataURL)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastStatus = resp.StatusCode
			resp.Body.Close()
			continue
		}
		var asm authorizationServerMetadata
		if err := json.NewDecoder(resp.Body).Decode(&asm); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decode auth server metadata: %w", err)
		}
		resp.Body.Close()
		return &asm, nil
	}

	return nil, fmt.Errorf("auth server metadata: HTTP %d (tried %d URLs)", lastStatus, len(candidates))
}

// dynamicRegister performs OAuth 2.0 Dynamic Client Registration (RFC7591).
func (om *oauthManager) dynamicRegister(registrationEndpoint, redirectURI string) (clientID string, err error) {
	body := map[string]interface{}{
		"client_name":                "Gino",
		"redirect_uris":             []string{redirectURI},
		"grant_types":               []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_method": "none",
	}
	bodyBytes, _ := json.Marshal(body)

	resp, err := om.httpClient.Post(registrationEndpoint, "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("dynamic registration: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("dynamic registration: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var regResp struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&regResp); err != nil {
		return "", fmt.Errorf("decode registration response: %w", err)
	}

	if regResp.ClientID == "" {
		return "", fmt.Errorf("dynamic registration: no client_id in response")
	}

	return regResp.ClientID, nil
}

// beginAuth performs discovery, PKCE, and registration, then builds the authorization URL.
// It stores the pending auth state and returns the URL the user should visit.
func (om *oauthManager) beginAuth() (string, error) {
	redirectURI := "http://localhost:1/callback"

	// Step 1: Discover protected resource metadata
	prm, err := om.discoverProtectedResource("")
	if err != nil {
		return "", fmt.Errorf("resource discovery: %w", err)
	}

	// Step 2: Discover authorization server metadata (use first auth server)
	authServerURL := prm.AuthorizationServers[0]
	asm, err := om.discoverAuthServer(authServerURL)
	if err != nil {
		return "", fmt.Errorf("auth server discovery: %w", err)
	}

	// Step 3: Dynamic client registration (if supported)
	clientID := ""
	if asm.RegistrationEndpoint != "" {
		clientID, err = om.dynamicRegister(asm.RegistrationEndpoint, redirectURI)
		if err != nil {
			log.Printf("mcp oauth %q: dynamic registration failed: %v, will try without client_id", om.serverName, err)
		}
	}

	// Step 4: Generate PKCE
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return "", fmt.Errorf("pkce: %w", err)
	}

	// Step 5: Generate state
	state, err := generateState()
	if err != nil {
		return "", fmt.Errorf("state: %w", err)
	}

	// Step 6: Build authorization URL
	params := url.Values{}
	if clientID != "" {
		params.Set("client_id", clientID)
	}
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)
	if len(prm.ScopesSupported) > 0 {
		params.Set("scope", strings.Join(prm.ScopesSupported, " "))
	}

	authURL := asm.AuthorizationEndpoint + "?" + params.Encode()

	// Step 7: Store pending state
	om.store.SetPending(om.serverURL, pendingAuth{
		CodeVerifier:  verifier,
		State:         state,
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		TokenEndpoint: asm.TokenEndpoint,
		CreatedAt:     time.Now(),
	})

	return authURL, nil
}

// completeAuth exchanges the authorization code for a token.
// redirectURL is the full URL the user was redirected to (which failed to load).
func (om *oauthManager) completeAuth(redirectURL string) error {
	pa, ok := om.store.GetPending(om.serverURL)
	if !ok {
		return fmt.Errorf("no pending OAuth flow for %q — call beginAuth first", om.serverName)
	}

	// Parse the redirect URL to extract code and state
	parsed, err := url.Parse(redirectURL)
	if err != nil {
		return fmt.Errorf("parse redirect URL: %w", err)
	}

	code := parsed.Query().Get("code")
	if code == "" {
		return fmt.Errorf("no 'code' parameter in redirect URL — authentication may have failed")
	}

	returnedState := parsed.Query().Get("state")
	if returnedState != "" && returnedState != pa.State {
		om.store.ClearPending(om.serverURL)
		return fmt.Errorf("state mismatch — possible CSRF attack, aborting")
	}

	// Exchange code for token
	token, err := om.exchangeCode(pa.TokenEndpoint, code, pa.CodeVerifier, pa.RedirectURI, pa.ClientID)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}

	// Store token and clear pending state
	om.store.SetToken(om.serverURL, *token)
	om.store.ClearPending(om.serverURL)

	log.Printf("mcp oauth %q: authentication successful, token cached", om.serverName)
	return nil
}

// exchangeCode exchanges an authorization code for an access token.
func (om *oauthManager) exchangeCode(tokenEndpoint, code, verifier, redirectURI, clientID string) (*Token, error) {
	params := url.Values{}
	params.Set("grant_type", "authorization_code")
	params.Set("code", code)
	params.Set("code_verifier", verifier)
	params.Set("redirect_uri", redirectURI)
	if clientID != "" {
		params.Set("client_id", clientID)
	}

	resp, err := om.httpClient.PostForm(tokenEndpoint, params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}

	token := &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
	}
	if tr.ExpiresIn > 0 {
		token.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}

	return token, nil
}

// refreshToken exchanges a refresh token for a new access token.
func (om *oauthManager) refreshToken(token *Token) (*Token, error) {
	pa, ok := om.store.GetPending(om.serverURL)
	if !ok {
		// We need the token endpoint — try to rediscover
		prm, err := om.discoverProtectedResource("")
		if err != nil {
			return nil, fmt.Errorf("resource discovery for refresh: %w", err)
		}
		asm, err := om.discoverAuthServer(prm.AuthorizationServers[0])
		if err != nil {
			return nil, fmt.Errorf("auth server discovery for refresh: %w", err)
		}
		pa.TokenEndpoint = asm.TokenEndpoint
	}

	if token.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token available")
	}

	params := url.Values{}
	params.Set("grant_type", "refresh_token")
	params.Set("refresh_token", token.RefreshToken)
	if pa.ClientID != "" {
		params.Set("client_id", pa.ClientID)
	}

	resp, err := om.httpClient.PostForm(pa.TokenEndpoint, params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("refresh: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}

	newToken := &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
	}
	if tr.ExpiresIn > 0 {
		newToken.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	// Preserve old refresh token if server doesn't return a new one
	if newToken.RefreshToken == "" {
		newToken.RefreshToken = token.RefreshToken
	}

	return newToken, nil
}

// parseWWWAuthenticate extracts the resource_metadata URL from a WWW-Authenticate header.
// Format per RFC9728: Bearer resource_metadata="https://example.com/.well-known/..."
func parseWWWAuthenticate(header string) string {
	// Remove "Bearer " prefix
	header = strings.TrimPrefix(header, "Bearer ")
	// Look for resource_metadata="..."
	parts := strings.Split(header, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "resource_metadata=") {
			val := strings.TrimPrefix(part, "resource_metadata=")
			val = strings.Trim(val, `"`)
			return val
		}
	}
	return ""
}

/*** OAuth pending registry ***/

// pendingOAuthRegistry holds ErrOAuthRequired errors that haven't been surfaced
// to the user yet. This allows the agent loop to register them at startup, and
// the mcp_auth tool to retrieve them.
var (
	pendingOAuth   = make(map[string]*ErrOAuthRequired)
	pendingOAuthMu sync.RWMutex
)

// SetOAuthPending registers a pending OAuth requirement for a server name.
func SetOAuthPending(serverName string, err *ErrOAuthRequired) {
	pendingOAuthMu.Lock()
	defer pendingOAuthMu.Unlock()
	pendingOAuth[serverName] = err
}

// GetOAuthPending retrieves a pending OAuth requirement for a server name.
func GetOAuthPending(serverName string) (*ErrOAuthRequired, bool) {
	pendingOAuthMu.RLock()
	defer pendingOAuthMu.RUnlock()
	e, ok := pendingOAuth[serverName]
	return e, ok
}

// ClearOAuthPending removes a pending OAuth requirement.
func ClearOAuthPending(serverName string) {
	pendingOAuthMu.Lock()
	defer pendingOAuthMu.Unlock()
	delete(pendingOAuth, serverName)
}

// AllPendingOAuth returns all pending OAuth requirements.
func AllPendingOAuth() map[string]*ErrOAuthRequired {
	pendingOAuthMu.RLock()
	defer pendingOAuthMu.RUnlock()
	result := make(map[string]*ErrOAuthRequired, len(pendingOAuth))
	for k, v := range pendingOAuth {
		result[k] = v
	}
	return result
}

// CompleteAuthForServer completes OAuth for a given server URL by accepting
// the redirect URL from the user. Returns nil on success.
func CompleteAuthForServer(serverName, serverURL, redirectURL string, headers map[string]string, tokenStore *TokenStore) error {
	om := newOAuthManager(serverURL, serverName, tokenStore)
	if err := om.completeAuth(redirectURL); err != nil {
		return err
	}
	return nil
}
