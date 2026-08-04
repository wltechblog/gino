package api

import (
	"html/template"
	"strings"
	"sync"
)

// ─── Embedded Admin Templates ─────────────────────────────────────────────────

var (
	adminTemplates     *template.Template
	adminTemplatesOnce sync.Once
)

func getAdminTemplates() *template.Template {
	adminTemplatesOnce.Do(func() {
		adminTemplates = template.Must(template.New("").Funcs(template.FuncMap{
			"join":      strings.Join,
			"hasPrefix": strings.HasPrefix,
			"lower":     strings.ToLower,
			"formatTime": formatTimeHelper,
			"dict":      dictHelper,
		}).Parse(adminTemplatesRaw))
	})
	return adminTemplates
}

func formatTimeHelper(t interface{}) string {
	type formatter interface{ Format(string) string }
	if f, ok := t.(formatter); ok && f != nil {
		return f.Format("2006-01-02 15:04")
	}
	return "—"
}

func dictHelper(pairs ...interface{}) map[string]interface{} {
	m := make(map[string]interface{})
	for i := 0; i+1 < len(pairs); i += 2 {
		key, _ := pairs[i].(string)
		m[key] = pairs[i+1]
	}
	return m
}

const adminCSS = `
:root {
  --bg: #0f1117; --surface: #1a1d27; --surface2: #242833; --border: #2e3240;
  --text: #e4e4e7; --text2: #9ca3af; --accent: #6366f1; --accent2: #818cf8;
  --success: #22c55e; --danger: #ef4444; --warn: #f59e0b; --radius: 8px;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background: var(--bg); color: var(--text); line-height: 1.6; font-size: 14px; }
a { color: var(--accent2); text-decoration: none; }
a:hover { text-decoration: underline; }
code { background: var(--surface2); padding: 2px 6px; border-radius: 4px; font-size: 13px; }
.admin-layout { display: flex; min-height: 100vh; }
.sidebar { width: 220px; background: var(--surface); border-right: 1px solid var(--border); position: fixed; height: 100vh; overflow-y: auto; }
.sidebar-brand { padding: 20px 16px; font-size: 18px; font-weight: 700; border-bottom: 1px solid var(--border); }
.sidebar-brand span { color: var(--accent2); }
.nav-item { display: block; padding: 10px 16px; color: var(--text2); transition: all 0.15s; border-left: 3px solid transparent; }
.nav-item:hover { background: var(--surface2); color: var(--text); text-decoration: none; }
.nav-item.active { color: var(--accent2); background: var(--surface2); border-left-color: var(--accent); }
.nav-section { padding: 8px 16px; font-size: 11px; text-transform: uppercase; letter-spacing: 1px; color: var(--text2); margin-top: 12px; }
.main-content { margin-left: 220px; padding: 32px; flex: 1; min-width: 0; }
.page-header { margin-bottom: 24px; }
.page-header h1 { font-size: 22px; font-weight: 600; }
.page-header .subtitle { color: var(--text2); font-size: 13px; margin-top: 2px; }
.card { background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius); padding: 20px; margin-bottom: 16px; }
.card-title { font-size: 14px; font-weight: 600; color: var(--text2); margin-bottom: 12px; text-transform: uppercase; letter-spacing: 0.5px; }
.stats-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap: 16px; margin-bottom: 24px; }
.stat-card { background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius); padding: 20px; }
.stat-value { font-size: 28px; font-weight: 700; }
.stat-label { font-size: 12px; color: var(--text2); text-transform: uppercase; letter-spacing: 0.5px; margin-top: 4px; }
table { width: 100%; border-collapse: collapse; }
th { text-align: left; padding: 10px 12px; border-bottom: 1px solid var(--border); font-size: 12px; text-transform: uppercase; letter-spacing: 0.5px; color: var(--text2); }
td { padding: 10px 12px; border-bottom: 1px solid var(--border); }
tr:hover td { background: var(--surface2); }
.badge { display: inline-block; padding: 2px 8px; border-radius: 12px; font-size: 11px; font-weight: 600; }
.badge-admin { background: rgba(99,102,241,0.15); color: var(--accent2); }
.badge-tier { background: rgba(34,197,94,0.15); color: var(--success); }
.badge-env { background: rgba(245,158,11,0.15); color: var(--warn); }
.form-group { margin-bottom: 16px; }
.form-group label { display: block; font-size: 13px; color: var(--text2); margin-bottom: 4px; }
.form-group input, .form-group select, .form-group textarea { width: 100%; background: var(--bg); border: 1px solid var(--border); color: var(--text); padding: 8px 12px; border-radius: var(--radius); font-size: 14px; font-family: inherit; }
.form-group input:focus, .form-group select:focus, .form-group textarea:focus { outline: none; border-color: var(--accent); }
.form-group textarea { min-height: 80px; resize: vertical; font-family: 'SF Mono', monospace; font-size: 13px; }
.form-group .hint { font-size: 11px; color: var(--text2); margin-top: 4px; }
.form-row { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
.btn { display: inline-block; padding: 8px 16px; border-radius: var(--radius); border: none; font-size: 14px; font-weight: 500; cursor: pointer; transition: all 0.15s; font-family: inherit; }
.btn-primary { background: var(--accent); color: #fff; }
.btn-primary:hover { background: var(--accent2); }
.btn-danger { background: var(--danger); color: #fff; }
.btn-danger:hover { opacity: 0.85; }
.btn-ghost { background: transparent; color: var(--text2); border: 1px solid var(--border); }
.btn-ghost:hover { background: var(--surface2); color: var(--text); }
.btn-sm { padding: 4px 10px; font-size: 12px; }
.login-container { display: flex; justify-content: center; align-items: center; min-height: 100vh; padding: 20px; }
.login-box { background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius); padding: 40px; max-width: 400px; width: 100%; }
.login-box h1 { font-size: 24px; margin-bottom: 8px; }
.login-box .subtitle { color: var(--text2); margin-bottom: 24px; }
.login-box .logo { font-size: 32px; text-align: center; margin-bottom: 16px; }
.alert { padding: 12px 16px; border-radius: var(--radius); margin-bottom: 16px; font-size: 13px; }
.alert-error { background: rgba(239,68,68,0.1); border: 1px solid var(--danger); color: var(--danger); }
.empty-state { text-align: center; padding: 48px; color: var(--text2); }
.actions-cell { white-space: nowrap; }
.actions-cell form { display: inline; }
.mono { font-family: 'SF Mono', 'Fira Code', monospace; font-size: 13px; }
.checkbox-group { display: flex; align-items: center; gap: 8px; }
.checkbox-group input { width: auto; }
`

const adminSidebar = `
<div class="admin-layout">
  <div class="sidebar">
    <div class="sidebar-brand">⚙️ Gino <span>Admin</span></div>
    <a class="nav-item {{if eq .Active "dashboard"}}active{{end}}" href="/admin/">📊 Dashboard</a>
    <div class="nav-section">Management</div>
    <a class="nav-item {{if eq .Active "users"}}active{{end}}" href="/admin/users">👥 Users</a>
    <a class="nav-item {{if eq .Active "tiers"}}active{{end}}" href="/admin/tiers">⭐ Tiers</a>
    <a class="nav-item {{if eq .Active "mcp"}}active{{end}}" href="/admin/mcp">🔌 MCP Servers</a>
    <a class="nav-item {{if eq .Active "providers"}}active{{end}}" href="/admin/providers">🤖 Providers</a>
    <div class="nav-section">System</div>
    <a class="nav-item" href="/api/v1/health" target="_blank">💓 Health</a>
    <a class="nav-item" href="/api/v1/info" target="_blank">ℹ️ Info</a>
    <a class="nav-item" href="/admin/logout">🚪 Logout</a>
  </div>
  <div class="main-content">
    {{if .ErrorMessage}}
    <div class="alert alert-error">⚠️ {{.ErrorMessage}}</div>
    {{end}}
`

const adminSidebarEnd = `
  </div>
</div>
`

const adminTemplatesRaw = `
{{define "login"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Gino Admin — Login</title>
<style>` + adminCSS + `</style>
</head>
<body>
<div class="login-container">
  <div class="login-box">
    <div class="logo">⚙️</div>
    <h1>Gino Admin</h1>
    <div class="subtitle">Enter your admin token to continue</div>
    {{if .ErrorMessage}}
    <div class="alert alert-error">{{.ErrorMessage}}</div>
    {{end}}
    <form method="POST" action="/admin/login">
      <div class="form-group">
        <input type="password" name="token" placeholder="Admin token" autofocus>
      </div>
      <button type="submit" class="btn btn-primary" style="width:100%">Login</button>
    </form>
  </div>
</div>
</body>
</html>
{{end}}

{{define "dashboard"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Gino Admin — Dashboard</title>
<style>` + adminCSS + `</style>
</head>
<body>
` + adminSidebar + `
    <div class="page-header">
      <h1>📊 Dashboard</h1>
      <div class="subtitle">System overview</div>
    </div>
    {{if .Dashboard}}
    <div class="stats-grid">
      <div class="stat-card">
        <div class="stat-value">{{.Dashboard.Users}}</div>
        <div class="stat-label">Total Users</div>
      </div>
      <div class="stat-card">
        <div class="stat-value">{{.Dashboard.Admins}}</div>
        <div class="stat-label">Admins</div>
      </div>
      <div class="stat-card">
        <div class="stat-value">{{.Dashboard.Tiers}}</div>
        <div class="stat-label">Tiers</div>
      </div>
      <div class="stat-card">
        <div class="stat-value">{{.Dashboard.MCPServers}}</div>
        <div class="stat-label">MCP Servers</div>
      </div>
      <div class="stat-card">
        <div class="stat-value">{{.Dashboard.Providers}}</div>
        <div class="stat-label">Providers</div>
      </div>
    </div>
    {{end}}
` + adminSidebarEnd + `
</body>
</html>
{{end}}

{{define "users"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Gino Admin — Users</title>
<style>` + adminCSS + `</style>
</head>
<body>
` + adminSidebar + `
    <div class="page-header">
      <h1>👥 Users</h1>
      <div class="subtitle">{{len .Users}} users configured</div>
    </div>
    <div style="margin-bottom:16px">
      <button class="btn btn-primary" onclick="document.getElementById('add-user').style.display='block'">+ Add User</button>
    </div>
    <div id="add-user" style="display:none;margin-bottom:24px">
      <div class="card">
        <div class="card-title">New User</div>
        <form method="POST" action="/admin/users/action">
          <input type="hidden" name="action" value="create">
          <div class="form-row">
            <div class="form-group"><label>User ID</label><input name="id" required placeholder="e.g. alice"></div>
            <div class="form-group"><label>Display Name</label><input name="displayName" placeholder="Alice Smith"></div>
          </div>
          <div class="form-row">
            <div class="form-group">
              <label>Tier</label>
              <select name="tier">
                {{range .TierNames}}<option value="{{.}}">{{.}}</option>{{end}}
              </select>
            </div>
            <div class="form-group"><label>Token</label><input name="token" placeholder="auto-generated if blank"></div>
          </div>
          <div class="form-group">
            <label>Channels</label>
            <textarea name="channels" placeholder="telegram:123456&#10;discord:789012"></textarea>
            <div class="hint">One per line, format: channel:ID</div>
          </div>
          <div class="form-group">
            <label>Workspace Override</label>
            <input name="workspace" placeholder="/workspaces/alice (blank = default)">
          </div>
          <div class="form-group checkbox-group">
            <input type="checkbox" name="admin" id="admin-chk"><label for="admin-chk" style="margin:0">Admin privileges</label>
          </div>
          <button type="submit" class="btn btn-primary">Create User</button>
          <button type="button" class="btn btn-ghost" onclick="document.getElementById('add-user').style.display='none'">Cancel</button>
        </form>
      </div>
    </div>
    <table>
      <thead><tr><th>ID</th><th>Name</th><th>Tier</th><th>Admin</th><th>Channels</th><th>Actions</th></tr></thead>
      <tbody>
      {{range .Users}}
        <tr>
          <td class="mono">{{.ID}}</td>
          <td>{{if .DisplayName}}{{.DisplayName}}{{else}}—{{end}}</td>
          <td><span class="badge badge-tier">{{.Tier}}</span></td>
          <td>{{if .Admin}}<span class="badge badge-admin">admin</span>{{else}}—{{end}}</td>
          <td>{{range $k, $v := .Channels}}<code>{{$k}}:{{$v}}</code> {{end}}</td>
          <td class="actions-cell">
            <a href="/admin/users/{{.ID}}" class="btn btn-ghost btn-sm">Edit</a>
            <form method="POST" action="/admin/users/action" onsubmit="return confirm('Delete user {{.ID}}?')">
              <input type="hidden" name="action" value="delete"><input type="hidden" name="id" value="{{.ID}}">
              <button type="submit" class="btn btn-danger btn-sm">Delete</button>
            </form>
          </td>
        </tr>
      {{end}}
      </tbody>
    </table>
` + adminSidebarEnd + `
</body>
</html>
{{end}}

{{define "user_edit"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Gino Admin — Edit User</title>
<style>` + adminCSS + `</style>
</head>
<body>
` + adminSidebar + `
    <div class="page-header">
      <h1>✏️ Edit User</h1>
      <div class="subtitle">Modify user configuration</div>
    </div>
    {{if .EditUser}}
    <div class="card" style="max-width:600px">
      <form method="POST" action="/admin/users/action">
        <input type="hidden" name="action" value="update">
        <input type="hidden" name="id" value="{{.EditUser.ID}}">
        <div class="form-group"><label>User ID</label><input value="{{.EditUser.ID}}" disabled></div>
        <div class="form-group"><label>Display Name</label><input name="displayName" value="{{.EditUser.DisplayName}}"></div>
        <div class="form-row">
          <div class="form-group">
            <label>Tier</label>
            <select name="tier">
              {{range $.TierNames}}<option value="{{.}}" {{if eq $.EditUser.Tier .}}selected{{end}}>{{.}}</option>{{end}}
            </select>
          </div>
          <div class="form-group"><label>Token</label><input name="token" value="{{.EditUser.Workspace}}" placeholder="unchanged if blank"></div>
        </div>
        <div class="form-group">
          <label>Channels</label>
          <textarea name="channels" placeholder="telegram:123456">{{range $k, $v := .EditUser.Channels}}{{$k}}:{{$v}}
{{end}}</textarea>
        </div>
        <div class="form-group"><label>Workspace Override</label><input name="workspace" value="{{.EditUser.Workspace}}"></div>
        <div class="form-group checkbox-group">
          <input type="checkbox" name="admin" id="admin-chk" {{if .EditUser.Admin}}checked{{end}}><label for="admin-chk" style="margin:0">Admin privileges</label>
        </div>
        <button type="submit" class="btn btn-primary">Save Changes</button>
        <a href="/admin/users" class="btn btn-ghost">Cancel</a>
      </form>
    </div>
    {{end}}
` + adminSidebarEnd + `
</body>
</html>
{{end}}

{{define "tiers"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Gino Admin — Tiers</title>
<style>` + adminCSS + `</style>
</head>
<body>
` + adminSidebar + `
    <div class="page-header">
      <h1>⭐ Tiers</h1>
      <div class="subtitle">{{len .Tiers}} tiers configured</div>
    </div>
    <div style="margin-bottom:16px">
      <button class="btn btn-primary" onclick="document.getElementById('add-tier').style.display='block'">+ Add Tier</button>
    </div>
    <div id="add-tier" style="display:none;margin-bottom:24px">
      <div class="card">
        <div class="card-title">New Tier</div>
        <form method="POST" action="/admin/tiers/action">
          <input type="hidden" name="action" value="create">
          <div class="form-row">
            <div class="form-group"><label>Name</label><input name="name" required placeholder="e.g. pro"></div>
            <div class="form-group"><label>Model</label><input name="model" placeholder="glm-5.2"></div>
          </div>
          <div class="form-row">
            <div class="form-group"><label>Max Tool Iterations</label><input name="maxToolIterations" type="number" value="20"></div>
            <div class="form-group"><label>Max Context Tokens</label><input name="maxContextTokens" type="number" value="0"></div>
          </div>
          <div class="form-row">
            <div class="form-group"><label>Rate Limit / Hour</label><input name="rateLimitPerHour" type="number" value="0"></div>
            <div class="form-group"><label>Rate Limit / Day</label><input name="rateLimitPerDay" type="number" value="0"></div>
          </div>
          <div class="form-group">
            <label>Allowed Tools</label>
            <input name="allowedTools" placeholder="web_search, brain_search, filesystem">
            <div class="hint">Comma-separated, blank = all tools</div>
          </div>
          <div class="form-group">
            <label>Disable Tools</label>
            <input name="disableTools" placeholder="exec, filesystem">
          </div>
          <button type="submit" class="btn btn-primary">Create Tier</button>
          <button type="button" class="btn btn-ghost" onclick="document.getElementById('add-tier').style.display='none'">Cancel</button>
        </form>
      </div>
    </div>
    <table>
      <thead><tr><th>Name</th><th>Model</th><th>Iterations</th><th>Rate/Hr</th><th>Rate/Day</th><th>Users</th><th>Actions</th></tr></thead>
      <tbody>
      {{range .Tiers}}
        <tr>
          <td class="mono">{{.Name}}</td>
          <td>{{if .Model}}{{.Model}}{{else}}<span style="color:var(--text2)">default</span>{{end}}</td>
          <td>{{if .MaxToolIterations}}{{.MaxToolIterations}}{{else}}—{{end}}</td>
          <td>{{if .RateLimitPerHour}}{{.RateLimitPerHour}}{{else}}∞{{end}}</td>
          <td>{{if .RateLimitPerDay}}{{.RateLimitPerDay}}{{else}}∞{{end}}</td>
          <td>{{.UserCount}}</td>
          <td class="actions-cell">
            <a href="/admin/tiers/{{.Name}}" class="btn btn-ghost btn-sm">Edit</a>
            <form method="POST" action="/admin/tiers/action" onsubmit="return confirm('Delete tier {{.Name}}?')">
              <input type="hidden" name="action" value="delete"><input type="hidden" name="name" value="{{.Name}}">
              <button type="submit" class="btn btn-danger btn-sm">Delete</button>
            </form>
          </td>
        </tr>
      {{end}}
      </tbody>
    </table>
` + adminSidebarEnd + `
</body>
</html>
{{end}}

{{define "tier_edit"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Gino Admin — Edit Tier</title>
<style>` + adminCSS + `</style>
</head>
<body>
` + adminSidebar + `
    <div class="page-header">
      <h1>✏️ Edit Tier</h1>
      <div class="subtitle">Modify tier configuration</div>
    </div>
    {{if .EditTier}}
    <div class="card" style="max-width:600px">
      <form method="POST" action="/admin/tiers/action">
        <input type="hidden" name="action" value="update">
        <input type="hidden" name="name" value="{{.EditTier.Name}}">
        <div class="form-group"><label>Name</label><input value="{{.EditTier.Name}}" disabled></div>
        <div class="form-group"><label>Model Override</label><input name="model" value="{{.EditTier.Model}}" placeholder="blank = use global default"></div>
        <div class="form-row">
          <div class="form-group"><label>Max Tool Iterations</label><input name="maxToolIterations" type="number" value="{{.EditTier.MaxToolIterations}}"></div>
          <div class="form-group"><label>Max Context Tokens</label><input name="maxContextTokens" type="number" value="{{.EditTier.MaxContextTokens}}"></div>
        </div>
        <div class="form-row">
          <div class="form-group"><label>Rate Limit / Hour</label><input name="rateLimitPerHour" type="number" value="{{.EditTier.RateLimitPerHour}}"></div>
          <div class="form-group"><label>Rate Limit / Day</label><input name="rateLimitPerDay" type="number" value="{{.EditTier.RateLimitPerDay}}"></div>
        </div>
        <div class="form-group">
          <label>Allowed Tools</label>
          <input name="allowedTools" value="{{join .EditTier.AllowedTools ", "}}">
          <div class="hint">Comma-separated, blank = all tools</div>
        </div>
        <div class="form-group">
          <label>Disable Tools</label>
          <input name="disableTools" value="{{join .EditTier.DisableTools ", "}}">
        </div>
        <button type="submit" class="btn btn-primary">Save Changes</button>
        <a href="/admin/tiers" class="btn btn-ghost">Cancel</a>
      </form>
    </div>
    {{end}}
` + adminSidebarEnd + `
</body>
</html>
{{end}}

{{define "mcp"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Gino Admin — MCP Servers</title>
<style>` + adminCSS + `</style>
</head>
<body>
` + adminSidebar + `
    <div class="page-header">
      <h1>🔌 MCP Servers</h1>
      <div class="subtitle">{{len .MCPServers}} servers configured</div>
    </div>
    <div style="margin-bottom:16px">
      <button class="btn btn-primary" onclick="document.getElementById('add-mcp').style.display='block'">+ Add Server</button>
    </div>
    <div id="add-mcp" style="display:none;margin-bottom:24px">
      <div class="card">
        <div class="card-title">New MCP Server</div>
        <form method="POST" action="/admin/mcp/action">
          <input type="hidden" name="action" value="create">
          <div class="form-row">
            <div class="form-group"><label>Name</label><input name="name" required placeholder="e.g. weather-api"></div>
            <div class="form-group">
              <label>Scope (User)</label>
              <select name="userId">
                <option value="">Global (all users)</option>
                {{range .UserIDs}}<option value="{{.}}">{{.}}</option>{{end}}
              </select>
            </div>
          </div>
          <div class="form-group"><label>Command (stdio)</label><input name="command" placeholder="e.g. node /path/to/server.js"></div>
          <div class="form-group"><label>Arguments</label><input name="args" placeholder="comma-separated"></div>
          <div class="form-group"><label>URL (HTTP/SSE)</label><input name="url" placeholder="https://api.example.com/mcp"></div>
          <div class="form-group">
            <label>Environment Variables</label>
            <textarea name="env" placeholder="API_KEY=abc123&#10;ANOTHER_VAR=value"></textarea>
            <div class="hint">One per line, format: KEY=value — stored encrypted, never displayed</div>
          </div>
          <div class="form-group">
            <label>Headers</label>
            <textarea name="headers" placeholder="Authorization=Bearer token"></textarea>
            <div class="hint">One per line, format: Key=value</div>
          </div>
          <button type="submit" class="btn btn-primary">Add Server</button>
          <button type="button" class="btn btn-ghost" onclick="document.getElementById('add-mcp').style.display='none'">Cancel</button>
        </form>
      </div>
    </div>
    {{if .MCPServers}}
    <table>
      <thead><tr><th>Name</th><th>Scope</th><th>Type</th><th>Details</th><th>Env</th><th>Actions</th></tr></thead>
      <tbody>
      {{range .MCPServers}}
        <tr>
          <td class="mono">{{.Name}}</td>
          <td>{{if .UserID}}{{.UserID}}{{else}}<span style="color:var(--text2)">global</span>{{end}}</td>
          <td>{{if .URL}}HTTP{{else}}stdio{{end}}</td>
          <td>{{if .URL}}{{.URL}}{{else}}{{.Command}}{{if .Args}} {{join .Args " "}}{{end}}{{end}}</td>
          <td>{{if .HasEnv}}<span class="badge badge-env">has keys</span>{{else}}—{{end}}</td>
          <td class="actions-cell">
            <form method="POST" action="/admin/mcp/action" onsubmit="return confirm('Delete MCP server {{.Name}}?')">
              <input type="hidden" name="action" value="delete">
              <input type="hidden" name="name" value="{{.Name}}">
              <input type="hidden" name="userId" value="{{.UserID}}">
              <button type="submit" class="btn btn-danger btn-sm">Delete</button>
            </form>
          </td>
        </tr>
      {{end}}
      </tbody>
    </table>
    {{else}}
    <div class="empty-state">
      <div class="icon">🔌</div>
      <div>No MCP servers configured. Click "Add Server" to create one.</div>
    </div>
    {{end}}
` + adminSidebarEnd + `
</body>
</html>
{{end}}

{{define "error"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Gino Admin — Error</title>
<style>` + adminCSS + `</style>
</head>
<body>
` + adminSidebar + `
    <div class="page-header">
      <h1>❌ Error</h1>
    </div>
    <a href="/admin/" class="btn btn-ghost">← Back to Dashboard</a>
` + adminSidebarEnd + `
</body>
</html>
{{end}}

{{define "providers"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Gino Admin — Providers</title>
<style>` + adminCSS + `</style>
</head>
<body>
` + adminSidebar + `
    <div class="page-header">
      <h1>🤖 Providers</h1>
      <div class="subtitle">{{len .Providers}} LLM providers configured</div>
    </div>
    <div style="margin-bottom:16px">
      <a href="/admin/providers/new" class="btn btn-primary">+ Add Provider</a>
    </div>
    {{if .Providers}}
    <table>
      <thead><tr><th>Name</th><th>API Base</th><th>Primary</th><th>Key</th><th>Fallback</th><th>Models</th><th>Actions</th></tr></thead>
      <tbody>
      {{range .Providers}}
        <tr>
          <td class="mono">{{.Name}}</td>
          <td class="mono" style="font-size:12px">{{.APIBase}}</td>
          <td>{{if .IsPrimary}}<span class="badge badge-admin">primary</span>{{else}}—{{end}}</td>
          <td>{{if .HasAPIKey}}<span class="badge badge-env">set</span>{{else}}<span style="color:var(--danger)">missing</span>{{end}}</td>
          <td>{{if .IsFallback}}<span class="badge badge-tier">#{{.FallbackOrder}}</span>{{else}}—{{end}}</td>
          <td>{{range .Models}}<code>{{.Name}}</code>{{if .Default}} ★{{end}} {{end}}</td>
          <td class="actions-cell">
            <a href="/admin/providers/{{.Name}}" class="btn btn-ghost btn-sm">Edit</a>
            <form method="POST" action="/admin/providers/action" onsubmit="return confirm('Delete provider {{.Name}}?')">
              <input type="hidden" name="action" value="delete"><input type="hidden" name="name" value="{{.Name}}">
              <button type="submit" class="btn btn-danger btn-sm">Delete</button>
            </form>
          </td>
        </tr>
      {{end}}
      </tbody>
    </table>
    {{else}}
    <div class="empty-state">
      <div style="font-size:48px;margin-bottom:12px">🤖</div>
      <div>No providers configured. Add one to get started.</div>
    </div>
    {{end}}
` + adminSidebarEnd + `
</body>
</html>
{{end}}

{{define "provider_edit"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Gino Admin — {{.Title}}</title>
<style>` + adminCSS + `</style>
</head>
<body>
` + adminSidebar + `
    <div class="page-header">
      <h1>{{.Title}}</h1>
      <div class="subtitle">Configure LLM provider endpoint and models</div>
    </div>
    {{if .EditProvider}}
    <div class="card" style="max-width:700px">
      <form method="POST" action="/admin/providers/action">
        <input type="hidden" name="action" value="{{if eq .Title "Add Provider"}}create{{else}}update{{end}}">
        <div class="form-row">
          <div class="form-group">
            <label>Provider Name</label>
            <input name="name" value="{{.EditProvider.Name}}" required placeholder="e.g. primary, cheap-fast">
            <div class="hint">Unique identifier for this provider</div>
          </div>
          <div class="form-group">
            <label>API Base URL</label>
            <input name="apiBase" value="{{.EditProvider.APIBase}}" required placeholder="https://api.openai.com/v1">
          </div>
        </div>
        <div class="form-group">
          <label>API Key</label>
          <input name="apiKey" type="password" placeholder="{{if .EditProvider.HasAPIKey}}Enter new key to replace{{else}}Enter your API key{{end}}">
          <div class="hint">{{if .EditProvider.HasAPIKey}}Key is set — enter new value to change{{else}}Required for provider to function{{end}}</div>
        </div>
        <div class="form-row">
          <div class="form-group checkbox-group">
            <input type="checkbox" name="isPrimary" id="isPrimary" {{if .EditProvider.IsPrimary}}checked{{end}}>
            <label for="isPrimary" style="margin:0">Primary provider (default for all users)</label>
          </div>
          <div class="form-group checkbox-group">
            <input type="checkbox" name="isFallback" id="isFallback" {{if .EditProvider.IsFallback}}checked{{end}}>
            <label for="isFallback" style="margin:0">Fallback provider (used when primary fails)</label>
          </div>
        </div>
        <div class="form-row">
          <div class="form-group"><label>Max Tokens</label><input name="maxTokens" type="number" value="{{if .EditProvider.MaxTokens}}{{.EditProvider.MaxTokens}}{{else}}0{{end}}" placeholder="0 = unlimited"></div>
          <div class="form-group"><label>Timeout (seconds)</label><input name="timeoutS" type="number" value="{{if .EditProvider.TimeoutS}}{{.EditProvider.TimeoutS}}{{else}}0{{end}}" placeholder="0 = default"></div>
        </div>
        <div class="form-row">
          <div class="form-group"><label>Max Retries</label><input name="maxRetries" type="number" value="{{if .EditProvider.MaxRetries}}{{.EditProvider.MaxRetries}}{{else}}2{{end}}"></div>
          <div class="form-group"><label>Retry Base Wait (s)</label><input name="retryBaseWaitS" type="number" value="2"></div>
        </div>
        <div class="form-row">
          <div class="form-group"><label>Fallback Order</label><input name="fallbackOrder" type="number" value="{{if .EditProvider.FallbackOrder}}{{.EditProvider.FallbackOrder}}{{else}}0{{end}}"></div>
          <div class="form-group"><label>Recover After</label><input name="recoverAfter" value="{{.EditProvider.RecoverAfter}}" placeholder="e.g. 5m"></div>
        </div>
        <div class="form-group">
          <label>Models</label>
          <textarea name="models" rows="6" placeholder="glm-5.2|GPT-5.2|default&#10;glm-5v-turbo|Vision|vision&#10;cheap-fast|Fast Model">{{range .EditProvider.Models}}{{.Name}}{{if .Label}}|{{.Label}}{{end}}{{if .Vision}}|vision{{end}}{{if .Default}}|default{{end}}
{{end}}</textarea>
          <div class="hint">One per line: name|label|vision|default — label, vision, and default are optional</div>
        </div>
        <button type="submit" class="btn btn-primary">Save Provider</button>
        <a href="/admin/providers" class="btn btn-ghost">Cancel</a>
      </form>
    </div>
    {{end}}
` + adminSidebarEnd + `
</body>
</html>
{{end}}
`
