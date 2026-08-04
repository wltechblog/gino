package tenant

import (
	"testing"
	"time"
)

func TestTierIsToolAllowed(t *testing.T) {
	tests := []struct {
		name      string
		tier      Tier
		toolName  string
		want      bool
	}{
		{
			name:     "empty whitelist allows all",
			tier:     Tier{Name: "test"},
			toolName: "exec",
			want:     true,
		},
		{
			name:     "wildcard allows all",
			tier:     Tier{Name: "test", AllowedTools: []string{"*"}},
			toolName: "exec",
			want:     true,
		},
		{
			name:     "explicit allow",
			tier:     Tier{Name: "test", AllowedTools: []string{"filesystem", "brain_search"}},
			toolName: "filesystem",
			want:     true,
		},
		{
			name:     "not in whitelist",
			tier:     Tier{Name: "test", AllowedTools: []string{"filesystem", "brain_search"}},
			toolName: "exec",
			want:     false,
		},
		{
			name:     "blacklist blocks even if wildcard allows",
			tier:     Tier{Name: "test", AllowedTools: []string{"*"}, DisableTools: []string{"exec"}},
			toolName: "exec",
			want:     false,
		},
		{
			name:     "blacklist does not affect other tools",
			tier:     Tier{Name: "test", AllowedTools: []string{"*"}, DisableTools: []string{"exec"}},
			toolName: "filesystem",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.tier.IsToolAllowed(tt.toolName, nil)
			if got != tt.want {
				t.Errorf("IsToolAllowed(%q) = %v, want %v", tt.toolName, got, tt.want)
			}
		})
	}
}

func TestRateLimitPerHour(t *testing.T) {
	ctx := NewUserContext(
		UserConfig{ID: "test"},
		&Tier{Name: "test", RateLimitPerHour: 3},
		"/tmp",
	)

	for i := 0; i < 3; i++ {
		ok, _ := ctx.CanStartTurn()
		if !ok {
			t.Fatalf("turn %d should be allowed", i+1)
		}
		ctx.BeginTurn()
		ctx.EndTurn()
	}

	ok, reason := ctx.CanStartTurn()
	if ok {
		t.Fatal("4th turn should be rate limited")
	}
	if reason == "" {
		t.Fatal("expected rate limit reason")
	}
}

func TestRateLimitPerDay(t *testing.T) {
	ctx := NewUserContext(
		UserConfig{ID: "test"},
		&Tier{Name: "test", RateLimitPerDay: 2},
		"/tmp",
	)

	for i := 0; i < 2; i++ {
		ok, _ := ctx.CanStartTurn()
		if !ok {
			t.Fatalf("turn %d should be allowed", i+1)
		}
		ctx.BeginTurn()
		ctx.EndTurn()
	}

	ok, _ := ctx.CanStartTurn()
	if ok {
		t.Fatal("3rd turn should be rate limited")
	}
}

func TestRateLimitUnlimited(t *testing.T) {
	ctx := NewUserContext(
		UserConfig{ID: "test"},
		&Tier{Name: "test"}, // no limits set
		"/tmp",
	)

	for i := 0; i < 100; i++ {
		ok, _ := ctx.CanStartTurn()
		if !ok {
			t.Fatalf("turn %d should be allowed (unlimited)", i+1)
		}
		ctx.BeginTurn()
		ctx.EndTurn()
	}
}

func TestConcurrentTurnLimit(t *testing.T) {
	ctx := NewUserContext(
		UserConfig{ID: "test"},
		&Tier{Name: "test", MaxConcurrentTurns: 1},
		"/tmp",
	)

	ok, _ := ctx.CanStartTurn()
	if !ok {
		t.Fatal("first turn should be allowed")
	}
	ctx.BeginTurn()

	ok, reason := ctx.CanStartTurn()
	if ok {
		t.Fatal("second concurrent turn should be blocked")
	}
	if reason == "" {
		t.Fatal("expected concurrent turn limit reason")
	}

	ctx.EndTurn()

	ok, _ = ctx.CanStartTurn()
	if !ok {
		t.Fatal("turn after completion should be allowed")
	}
}

func TestUserManagerRegisterAndGet(t *testing.T) {
	dir := t.TempDir()
	m := NewUserManager(dir)
	m.RegisterTier(&Tier{Name: "free"})

	_, err := m.RegisterUser(UserConfig{
		ID:    "user1",
		Tier:  "free",
		Token: "tok123",
	})
	if err != nil {
		t.Fatalf("RegisterUser failed: %v", err)
	}

	ctx := m.Get("user1")
	if ctx == nil {
		t.Fatal("Get returned nil after RegisterUser")
	}
	if ctx.Config.ID != "user1" {
		t.Errorf("got ID %q, want %q", ctx.Config.ID, "user1")
	}
	if ctx.Tier.Name != "free" {
		t.Errorf("got tier %q, want %q", ctx.Tier.Name, "free")
	}

	// Verify workspace was created
	if ctx.WorkspacePath != dir+"/user1" {
		t.Errorf("workspace path = %q, want %q", ctx.WorkspacePath, dir+"/user1")
	}
}

func TestUserManagerGetByToken(t *testing.T) {
	dir := t.TempDir()
	m := NewUserManager(dir)
	m.RegisterTier(&Tier{Name: "free"})

	_, _ = m.RegisterUser(UserConfig{
		ID:    "user1",
		Tier:  "free",
		Token: "secret-token",
	})

	ctx, id := m.GetByToken("secret-token")
	if ctx == nil {
		t.Fatal("GetByToken returned nil for valid token")
	}
	if id != "user1" {
		t.Errorf("got ID %q, want %q", id, "user1")
	}

	ctx, id = m.GetByToken("wrong-token")
	if ctx != nil {
		t.Fatal("GetByToken should return nil for invalid token")
	}
}

func TestUserManagerGetByChannel(t *testing.T) {
	dir := t.TempDir()
	m := NewUserManager(dir)
	m.RegisterTier(&Tier{Name: "free"})

	_, _ = m.RegisterUser(UserConfig{
		ID:      "user1",
		Tier:    "free",
		Channels: map[string]string{"telegram": "12345"},
	})

	ctx, id := m.GetByChannel("telegram", "12345")
	if ctx == nil {
		t.Fatal("GetByChannel returned nil")
	}
	if id != "user1" {
		t.Errorf("got ID %q, want %q", id, "user1")
	}
}

func TestUserManagerUnknownTier(t *testing.T) {
	dir := t.TempDir()
	m := NewUserManager(dir)

	_, err := m.RegisterUser(UserConfig{
		ID:   "user1",
		Tier: "nonexistent",
	})
	if err == nil {
		t.Fatal("RegisterUser should fail for unknown tier")
	}
}

func TestEvictionRemovesIdleUsers(t *testing.T) {
	dir := t.TempDir()
	m := NewUserManager(dir)
	m.SetEvictionTimeout(50 * time.Millisecond)

	m.RegisterTier(&Tier{Name: "free"})
	_, _ = m.RegisterUser(UserConfig{
		ID:    "user1",
		Tier:  "free",
		Token: "tok1",
	})

	if m.ActiveUsers() != 1 {
		t.Fatalf("expected 1 active user, got %d", m.ActiveUsers())
	}

	// Wait for eviction
	time.Sleep(200 * time.Millisecond)
	m.evictIdle()

	if m.ActiveUsers() != 0 {
		t.Fatalf("expected 0 active users after eviction, got %d", m.ActiveUsers())
	}
}

func TestEvictionSkipsActiveTurns(t *testing.T) {
	dir := t.TempDir()
	m := NewUserManager(dir)
	m.SetEvictionTimeout(50 * time.Millisecond)

	m.RegisterTier(&Tier{Name: "free"})
	_, _ = m.RegisterUser(UserConfig{
		ID:    "user1",
		Tier:  "free",
		Token: "tok1",
	})

	// Start a turn that doesn't end
	ctx := m.Get("user1")
	ctx.BeginTurn()

	time.Sleep(200 * time.Millisecond)
	m.evictIdle()

	if m.ActiveUsers() != 1 {
		t.Fatalf("active user should not be evicted, got %d active", m.ActiveUsers())
	}
}
