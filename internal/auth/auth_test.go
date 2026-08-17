package auth

import (
	"strings"
	"testing"
	"time"

	"iot-gateway-go/internal/store"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewManager(st, time.Hour)
}

func TestPrincipalHasScope(t *testing.T) {
	all := Principal{Scopes: []Scope{ScopeAll}}
	if !all.HasScope(ScopeDevicesRead) || !all.HasScope(ScopeClientsWrite) {
		t.Fatal("ScopeAll should match everything")
	}
	subset := Principal{Scopes: []Scope{ScopeDevicesRead, ScopeStatusRead}}
	if !subset.HasScope(ScopeDevicesRead) {
		t.Fatal("should have devices:read")
	}
	if subset.HasScope(ScopeDevicesWrite) {
		t.Fatal("should not have devices:write")
	}
}

func TestBootstrapAdminAndLogin(t *testing.T) {
	m := newTestManager(t)

	created, err := m.BootstrapAdmin()
	if err != nil || !created {
		t.Fatalf("bootstrap: created=%v err=%v", created, err)
	}
	// 二次调用幂等:已有用户则跳过
	created, err = m.BootstrapAdmin()
	if err != nil || created {
		t.Fatalf("second bootstrap should be no-op, created=%v err=%v", created, err)
	}

	token, p, err := m.Login(DefaultAdminUser, DefaultAdminPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if token == "" || p.ID != DefaultAdminUser {
		t.Fatalf("login result: token=%q principal=%+v", token, p)
	}
	if !p.MustChangePassword {
		t.Fatal("first login should require password change")
	}
	if !p.HasScope(ScopeAll) {
		t.Fatal("admin should have wildcard scope")
	}
}

func TestLoginWrongPassword(t *testing.T) {
	m := newTestManager(t)
	m.BootstrapAdmin()
	if _, _, err := m.Login(DefaultAdminUser, "wrong"); err != ErrAuthn {
		t.Fatalf("wrong password should return ErrAuthn, got %v", err)
	}
	if _, _, err := m.Login("nobody", "whatever"); err != ErrAuthn {
		t.Fatalf("unknown user should return ErrAuthn, got %v", err)
	}
}

func TestAuthenticateAndLogout(t *testing.T) {
	m := newTestManager(t)
	m.BootstrapAdmin()
	token, _, _ := m.Login(DefaultAdminUser, DefaultAdminPassword)

	if _, ok := m.Authenticate(token); !ok {
		t.Fatal("authenticate should succeed with valid token")
	}
	m.Logout(token)
	if _, ok := m.Authenticate(token); ok {
		t.Fatal("authenticate should fail after logout")
	}
	if _, ok := m.Authenticate("bogus"); ok {
		t.Fatal("authenticate should fail with bogus token")
	}
}

func TestChangePassword(t *testing.T) {
	m := newTestManager(t)
	m.BootstrapAdmin()

	if err := m.ChangePassword(DefaultAdminUser, "wrong-old", "newpass123"); err != ErrAuthn {
		t.Fatalf("wrong old password should fail, got %v", err)
	}
	if err := m.ChangePassword(DefaultAdminUser, DefaultAdminPassword, "newpass123"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	// 旧密码失效,新密码可登录,改密标志清除
	if _, _, err := m.Login(DefaultAdminUser, DefaultAdminPassword); err != ErrAuthn {
		t.Fatal("old password should no longer work")
	}
	_, p, err := m.Login(DefaultAdminUser, "newpass123")
	if err != nil {
		t.Fatalf("login with new password: %v", err)
	}
	if p.MustChangePassword {
		t.Fatal("must_change_password should be cleared")
	}
}

func TestClientAPIKeyFlow(t *testing.T) {
	m := newTestManager(t)

	c, key, err := m.CreateClient("mes-ro", "MES 只读", []string{"devices:read", "status:read"})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	if key == "" || c.APIKeyHash != hashAPIKey(key) {
		t.Fatal("api key or hash wrong")
	}
	if !strings.HasPrefix(key, "cgw_") {
		t.Fatalf("api key prefix: %q", key)
	}

	// 用明文 key 认证,得到绑定 scope
	p, ok := m.Authenticate(key)
	if !ok || p.Kind != kindClient || p.ID != "mes-ro" {
		t.Fatalf("authenticate client: ok=%v principal=%+v", ok, p)
	}
	if !p.HasScope(ScopeDevicesRead) || p.HasScope(ScopeDevicesWrite) {
		t.Fatalf("client scopes wrong: %+v", p.Scopes)
	}

	// 吊销后认证失败
	m.UpdateClient("mes-ro", []string{"devices:read"}, false)
	if _, ok := m.Authenticate(key); ok {
		t.Fatal("disabled client should not authenticate")
	}
}
