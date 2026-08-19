// Package auth 提供 API 的鉴权(认证)与授权(scope 访问控制)。
//
// 自研轻量 scope/RBAC:主体分"管理员(人)"与"三方 client(系统)"两类;授权粒度到接口,
// 每个接口声明所需 scope(如 devices:read),中间件校验主体是否持有该 scope。
// 管理员密码只存 bcrypt 哈希,三方 API Key 只存 SHA-256 哈希。详见 docs/authz.md。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"iot-gateway-go/internal/model"
	"iot-gateway-go/internal/store"
)

// Scope 是权限范围,形如 "resource:action"。管理员持有通配 "*"。
type Scope string

// 接口粒度 scope,与 docs/authz.md §3.1 矩阵对应。
const (
	ScopeAll              Scope = "*"
	ScopeConnectionsRead  Scope = "connections:read"
	ScopeConnectionsWrite Scope = "connections:write"
	ScopeDevicesRead      Scope = "devices:read"
	ScopeDevicesWrite     Scope = "devices:write"
	ScopeDevicesCommand   Scope = "devices:command"
	ScopeStatusRead       Scope = "status:read"
	ScopeValuesRead       Scope = "values:read"
	ScopeDriversRead      Scope = "drivers:read"
	ScopeClientsRead      Scope = "clients:read"
	ScopeClientsWrite     Scope = "clients:write"
	ScopeOutputsRead      Scope = "outputs:read"
	ScopeOutputsWrite     Scope = "outputs:write"
	ScopeGatewayRead      Scope = "gateway:read"
	ScopeGatewayWrite     Scope = "gateway:write"
)

// principalKind 区分主体类型。
type principalKind string

const (
	kindUser   principalKind = "user"
	kindClient principalKind = "client"
)

// Principal 是认证通过后的主体,携带其身份与权限。
type Principal struct {
	Kind               principalKind
	ID                 string
	Scopes             []Scope
	MustChangePassword bool
}

// HasScope 判断主体是否持有某 scope;通配 "*" 匹配一切。
func (p Principal) HasScope(scope Scope) bool {
	for _, s := range p.Scopes {
		if s == ScopeAll || s == scope {
			return true
		}
	}
	return false
}

// DefaultAdmin 是预置管理员的账号/初始口令。仅用于首次预置,首次登录强制改密。
const (
	DefaultAdminUser     = "admin"
	DefaultAdminPassword = "admin123"
)

// ErrAuthn 是认证失败(无凭证/凭证无效)的统一错误,中间件据此返回 401。
var ErrAuthn = errors.New("authentication failed")

// ErrAuthz 是授权失败(缺 scope)的错误,中间件据此返回 403。
var ErrAuthz = errors.New("insufficient scope")

// ErrPasswordChangeRequired 表示必须改密后才能访问业务接口(403)。
var ErrPasswordChangeRequired = errors.New("password change required")

// Manager 持有认证状态(内存 session)与用户/三方 client 的持久化引用。
// 并发安全,可被多个 HTTP 请求 goroutine 同时调用。
type Manager struct {
	st       *store.Store
	sessions *sessionStore
	ttl      time.Duration
}

func NewManager(st *store.Store, ttl time.Duration) *Manager {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Manager{st: st, sessions: newSessionStore(), ttl: ttl}
}

// BootstrapAdmin 在 gw_user 表为空时预置默认管理员(admin/admin123,须改密)。
// 幂等:已有用户则跳过。返回是否执行了预置。
func (m *Manager) BootstrapAdmin() (bool, error) {
	n, err := m.st.CountUsers()
	if err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	if n > 0 {
		return false, nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(DefaultAdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return false, fmt.Errorf("hash default password: %w", err)
	}
	u := model.User{ID: DefaultAdminUser, PasswordHash: string(hash), MustChangePassword: true, Enabled: true}
	if err := m.st.SaveUser(u); err != nil {
		return false, fmt.Errorf("bootstrap admin: %w", err)
	}
	return true, nil
}

// Login 校验用户名密码,成功后颁发 session token 并返回主体。
func (m *Manager) Login(username, password string) (token string, p Principal, err error) {
	u, err := m.st.GetUser(username)
	if err != nil {
		return "", Principal{}, ErrAuthn // 不区分"用户不存在/密码错",防枚举
	}
	if !u.Enabled {
		return "", Principal{}, ErrAuthn
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return "", Principal{}, ErrAuthn
	}
	token, err = randomToken()
	if err != nil {
		return "", Principal{}, err
	}
	m.sessions.put(token, username, m.ttl)
	return token, Principal{
		Kind:               kindUser,
		ID:                 u.ID,
		Scopes:             []Scope{ScopeAll},
		MustChangePassword: u.MustChangePassword,
	}, nil
}

// Logout 注销 session token。
func (m *Manager) Logout(token string) {
	m.sessions.delete(token)
}

// Authenticate 从 Bearer token 解析主体:先匹配 session(管理员),再匹配 API Key(三方)。
// 失败返回 (Principal{}, false)。
func (m *Manager) Authenticate(token string) (Principal, bool) {
	if token == "" {
		return Principal{}, false
	}
	if userID, ok := m.sessions.get(token); ok {
		u, err := m.st.GetUser(userID)
		if err != nil || !u.Enabled {
			return Principal{}, false
		}
		return Principal{
			Kind:               kindUser,
			ID:                 u.ID,
			Scopes:             []Scope{ScopeAll},
			MustChangePassword: u.MustChangePassword,
		}, true
	}
	if c, ok := m.authenticateClient(token); ok {
		scopes := make([]Scope, 0, len(c.Scopes))
		for _, s := range c.Scopes {
			scopes = append(scopes, Scope(s))
		}
		return Principal{Kind: kindClient, ID: c.ID, Scopes: scopes}, true
	}
	return Principal{}, false
}

func (m *Manager) authenticateClient(token string) (model.Client, bool) {
	hash := hashAPIKey(token)
	c, ok := m.st.GetClientByKeyHash(hash)
	if !ok || !c.Enabled {
		return model.Client{}, false
	}
	return c, true
}

// ChangePassword 校验旧密码后设置新密码,并清除首次改密标志。
func (m *Manager) ChangePassword(username, oldPassword, newPassword string) error {
	u, err := m.st.GetUser(username)
	if err != nil {
		return ErrAuthn
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(oldPassword)); err != nil {
		return ErrAuthn
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash new password: %w", err)
	}
	u.PasswordHash = string(hash)
	u.MustChangePassword = false
	if err := m.st.SaveUser(u); err != nil {
		return fmt.Errorf("save user: %w", err)
	}
	return nil
}

// CreateClient 生成三方 client:随机 API Key,存哈希,返回 client 与一次性明文 key。
func (m *Manager) CreateClient(id, name string, scopes []string) (model.Client, string, error) {
	key, err := randomAPIKey()
	if err != nil {
		return model.Client{}, "", err
	}
	c := model.Client{
		ID:         id,
		Name:       name,
		APIKeyHash: hashAPIKey(key),
		Scopes:     scopes,
		Enabled:    true,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := m.st.SaveClient(c); err != nil {
		return model.Client{}, "", err
	}
	return c, key, nil
}

// UpdateClient 更新三方 client 的 scope 与启用状态(不改 API Key)。
func (m *Manager) UpdateClient(id string, scopes []string, enabled bool) (model.Client, error) {
	c, err := m.st.GetClient(id)
	if err != nil {
		return model.Client{}, err
	}
	c.Scopes = scopes
	c.Enabled = enabled
	if err := m.st.SaveClient(c); err != nil {
		return model.Client{}, err
	}
	return c, nil
}

// hashAPIKey 计算 API Key 的 SHA-256 哈希(hex)。
func hashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// randomToken 生成 256-bit 随机 session token(hex)。
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// randomAPIKey 生成 256-bit 随机 API Key,带 cgw_ 前缀便于识别。
func randomAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand api key: %w", err)
	}
	return "cgw_" + hex.EncodeToString(b), nil
}

// ConstantTimeCompare 对外暴露时序安全比较(用于 API Key 或 token 的二次校验场景)。
func ConstantTimeCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---- 请求上下文 ----

type ctxKey struct{}

// WithPrincipal 把主体注入请求上下文,供中间件与 handler 读取。
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFromContext 从请求上下文取主体;未认证返回 false。
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// BearerFromHeader 从 Authorization 头提取 Bearer token;无/格式错返回空串。
func BearerFromHeader(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
