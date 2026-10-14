// Package openaioauth 实现官方文档定义的本地 Sign in with ChatGPT OAuth 流程。
package openaioauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	authorizeURL = "https://auth.openai.com/api/accounts/authorize"
	tokenURL     = "https://auth.openai.com/api/accounts/oauth/token"
	jwksURL      = "https://auth.openai.com/.well-known/jwks.json"
	resource     = "https://api.openai.com/v1"
	callbackPath = "/auth/openai/callback"
)

// Manager 管理一个本机 ChatGPT OAuth 账号及会轮换的访问凭证。
type Manager struct {
	codex   bool
	mu      sync.Mutex
	path    string
	name    string
	client  *http.Client
	token   credentials
	pending map[string]attempt
}

type credentials struct {
	AccountID    string    `json:"account_id,omitempty"`
	ClientID     string    `json:"client_id"`
	HostID       string    `json:"ext_agent_host_id"`
	Subject      string    `json:"subject"`
	Email        string    `json:"email,omitempty"`
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Scopes       string    `json:"scope"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type attempt struct {
	state, nonce, verifier, redirectURI, clientID string
	createdAt                                     time.Time
}

type tokenReply struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	ClientID     string `json:"client_id"`
}

// New 从磁盘读取并保护已保存的凭证，同时初始化本机 OAuth 回调状态。
func New(path, agentName string, client *http.Client) (*Manager, error) {
	if path == "" || agentName == "" || client == nil {
		return nil, errors.New("OAuth credential path, app name and HTTP client are required")
	}
	m := &Manager{path: path, name: agentName, client: client, pending: make(map[string]attempt)}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &m.token); err != nil {
			return nil, errors.New("OpenAI OAuth credential file is invalid")
		}
		if err := os.Chmod(path, 0600); err != nil {
			return nil, errors.New("cannot protect OpenAI OAuth credential file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("cannot read OpenAI OAuth credential file")
	}
	if m.token.HostID == "" {
		rawID := make([]byte, 16)
		if _, err := rand.Read(rawID); err != nil {
			return nil, err
		}
		rawID[6] = (rawID[6] & 0x0f) | 0x40
		rawID[8] = (rawID[8] & 0x3f) | 0x80
		id := hex.EncodeToString(rawID)
		m.token.HostID = "urn:uuid:" + id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
		if err := m.saveLocked(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Handler 返回实现 http.Handler 接口的处理器，提供登录、回调、状态和退出授权端点。
func (m *Manager) Handler() http.Handler { return http.HandlerFunc(m.serveHTTP) }

// serveHTTP 根据 URL 路径分发 OAuth 子功能，并将控制接口限制为本机访问。
func (m *Manager) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		http.Error(w, "OAuth is available only from this computer", http.StatusForbidden)
		return
	}
	if (r.URL.Path == "/auth/openai/start" || r.URL.Path == "/auth/openai/logout") && !sameOrigin(r) {
		http.Error(w, "request origin is invalid", http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/auth/openai/start":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		m.start(w, r)
	case callbackPath:
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		m.callbackHTTP(w, r)
	case "/auth/openai/status":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		m.status(w)
	case "/auth/openai/logout":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		m.logout(w, r)
	default:
		http.NotFound(w, r)
	}
}

// start 为这次登录生成 state、nonce 和 PKCE verifier，返回给浏览器打开的授权地址。
func (m *Manager) start(w http.ResponseWriter, r *http.Request) {
	if m.codex {
		http.Error(w, "Use server --login openai to connect this Codex provider", http.StatusBadRequest)
		return
	}
	callback, err := loopbackCallback(r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	state, err := randomValue(32)
	if err != nil {
		http.Error(w, "cannot start OAuth login", http.StatusInternalServerError)
		return
	}
	nonce, err := randomValue(32)
	if err != nil {
		http.Error(w, "cannot start OAuth login", http.StatusInternalServerError)
		return
	}
	verifier, err := randomValue(32)
	if err != nil {
		http.Error(w, "cannot start OAuth login", http.StatusInternalServerError)
		return
	}
	m.mu.Lock()
	clientID := m.token.ClientID
	if clientID == "" {
		clientID = "dynamic_agent_client"
	}
	now := time.Now()
	// Keep only the latest browser attempt so abandoned tabs cannot grow this map indefinitely.
	m.pending = make(map[string]attempt)
	a := attempt{state: state, nonce: nonce, verifier: verifier, redirectURI: callback, clientID: clientID, createdAt: now}
	m.pending[state] = a
	m.mu.Unlock()

	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {callback},
		"scope":                 {"openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"},
		"resource":              {resource},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge_method": {"S256"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"ext_agent_host_id":     {m.token.HostID},
	}
	if clientID == "dynamic_agent_client" {
		query.Set("agent_name_hint", m.name)
	} else if m.token.IDToken != "" {
		query.Set("id_token_hint", m.token.IDToken)
	}
	writeJSON(w, http.StatusOK, map[string]string{"authorization_url": authorizeURL + "?" + query.Encode()})
}

// callbackHTTP 校验 state、交换一次性授权码、验签 ID Token，并保存可刷新的凭证。
func (m *Manager) callbackHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	m.mu.Lock()
	a, ok := m.pending[state]
	delete(m.pending, state)
	m.mu.Unlock()
	if !ok || state == "" || time.Since(a.createdAt) > 10*time.Minute {
		http.Error(w, "OAuth state is invalid or expired; start login again", http.StatusBadRequest)
		return
	}
	if q.Get("error") != "" {
		http.Error(w, "OpenAI authorization was not completed", http.StatusBadRequest)
		return
	}
	code := q.Get("code")
	clientID := a.clientID
	if supplied := q.Get("client_id"); supplied != "" {
		if a.clientID != "dynamic_agent_client" && supplied != a.clientID {
			http.Error(w, "OAuth client does not match the pending account", http.StatusBadRequest)
			return
		}
		clientID = supplied
	}
	if code == "" || clientID == "" || clientID == "dynamic_agent_client" {
		http.Error(w, "OpenAI OAuth registration is incomplete", http.StatusBadRequest)
		return
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {a.verifier}, "redirect_uri": {a.redirectURI}, "resource": {resource}}
	var reply tokenReply
	if err := m.tokenRequest(r.Context(), form, &reply); err != nil {
		http.Error(w, "OpenAI token exchange failed", http.StatusBadGateway)
		return
	}
	if !hasScope(reply.Scope, "chatgpt.tokens.use.direct") || !hasScope(reply.Scope, "resource.invoke") || !hasScope(reply.Scope, "offline_access") || reply.AccessToken == "" || reply.RefreshToken == "" || reply.IDToken == "" || reply.ExpiresIn <= 0 || !strings.EqualFold(reply.TokenType, "Bearer") {
		http.Error(w, "OpenAI did not grant ChatGPT plan inference access", http.StatusForbidden)
		return
	}
	if reply.ClientID != "" && reply.ClientID != clientID {
		http.Error(w, "OpenAI returned credentials for a different OAuth client", http.StatusForbidden)
		return
	}
	claims, err := verifyIDToken(r.Context(), m.client, reply.IDToken, clientID, a.nonce)
	if err != nil {
		http.Error(w, "OpenAI identity token could not be verified", http.StatusBadGateway)
		return
	}
	m.mu.Lock()
	if m.token.Subject != "" && m.token.Subject != claims.Subject {
		m.mu.Unlock()
		http.Error(w, "the selected OpenAI account changed; start a new login", http.StatusForbidden)
		return
	}
	m.token.ClientID = clientID
	m.token.Subject = claims.Subject
	m.token.Email = claims.Email
	m.token.IDToken = reply.IDToken
	m.token.AccessToken = reply.AccessToken
	m.token.RefreshToken = reply.RefreshToken
	m.token.Scopes = reply.Scope
	m.token.ExpiresAt = time.Now().Add(time.Duration(reply.ExpiresIn) * time.Second)
	err = m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		http.Error(w, "could not save OpenAI credentials", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>授权完成</title><p>OpenAI 账号授权成功，可以关闭此窗口并返回工作台。</p><a href="/">返回工作台</a>`)
}

// status 只返回是否已连接和账户邮箱，不会把任何访问令牌发送给浏览器。
func (m *Manager) status(w http.ResponseWriter) {
	m.mu.Lock()
	connected := m.token.AccessToken != "" && m.token.RefreshToken != ""
	email := m.token.Email
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"connected": connected, "email": email, "cli_login": m.codex})
}

// logout 清除本地令牌并尝试向 OpenAI 撤销 refresh token；远端未确认时会提示用户。
func (m *Manager) logout(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	refresh := m.token.RefreshToken
	clientID := m.token.ClientID
	m.token.AccessToken, m.token.RefreshToken, m.token.IDToken = "", "", ""
	m.token.Email, m.token.Subject, m.token.Scopes = "", "", ""
	m.token.AccountID = ""
	m.token.ExpiresAt = time.Time{}
	err := m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		http.Error(w, "could not clear OpenAI credentials", http.StatusInternalServerError)
		return
	}
	if refresh != "" && !m.codex {
		form := url.Values{"token": {refresh}, "token_type_hint": {"refresh_token"}, "client_id": {clientID}}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://auth.openai.com/api/accounts/oauth/revoke", strings.NewReader(form.Encode()))
		if req != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if resp, err := m.client.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					http.Error(w, "local credentials were cleared, but OpenAI did not confirm remote revocation; disconnect this app in ChatGPT settings", http.StatusBadGateway)
					return
				}
			} else {
				http.Error(w, "local credentials were cleared, but remote revocation was not confirmed; disconnect this app in ChatGPT settings", http.StatusBadGateway)
				return
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// AccessToken 返回可用的 Bearer 令牌，并用互斥锁串行刷新，避免重复消费轮换令牌。
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	if m.codex {
		return m.codexAccessToken(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token.RefreshToken == "" {
		return "", errors.New("OpenAI OAuth login is required")
	}
	if time.Until(m.token.ExpiresAt) > 90*time.Second {
		return m.token.AccessToken, nil
	}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {m.token.ClientID}, "refresh_token": {m.token.RefreshToken}, "resource": {resource}}
	var reply tokenReply
	if err := m.tokenRequest(ctx, form, &reply); err != nil {
		return "", errors.New("OpenAI OAuth token refresh failed")
	}
	if reply.AccessToken == "" || reply.RefreshToken == "" || reply.ExpiresIn <= 0 || !strings.EqualFold(reply.TokenType, "Bearer") || !hasScope(reply.Scope, "chatgpt.tokens.use.direct") || !hasScope(reply.Scope, "resource.invoke") || !hasScope(reply.Scope, "offline_access") {
		return "", errors.New("OpenAI OAuth refresh did not return valid inference credentials")
	}
	m.token.AccessToken = reply.AccessToken
	m.token.RefreshToken = reply.RefreshToken
	m.token.Scopes = reply.Scope
	m.token.ExpiresAt = time.Now().Add(time.Duration(reply.ExpiresIn) * time.Second)
	if err := m.saveLocked(); err != nil {
		return "", errors.New("OpenAI OAuth credentials could not be saved")
	}
	return m.token.AccessToken, nil
}

// tokenRequest 发送表单编码的 OAuth 请求，只接受有大小上限的成功 JSON 响应。
func (m *Manager) tokenRequest(ctx context.Context, form url.Values, out *tokenReply) error {
	endpoint := tokenURL
	if m.codex {
		endpoint = codexTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OAuth endpoint returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || json.Unmarshal(data, out) != nil {
		return errors.New("OAuth endpoint returned invalid JSON")
	}
	return nil
}

// saveLocked 在调用方持有 m.mu 时原子替换凭证文件，并限制文件访问权限。
func (m *Manager) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.token, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".oauth-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

type identity struct{ Subject, Email string }

// verifyIDToken 校验 OIDC 签名、issuer、audience、nonce 和有效期，确保令牌属于当前登录。
func verifyIDToken(ctx context.Context, client *http.Client, raw, clientID, nonce string) (identity, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return identity{}, errors.New("invalid JWT")
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	var claims struct {
		Issuer    string      `json:"iss"`
		Subject   string      `json:"sub"`
		Audience  interface{} `json:"aud"`
		Expires   int64       `json:"exp"`
		NotBefore int64       `json:"nbf"`
		IssuedAt  int64       `json:"iat"`
		Nonce     string      `json:"nonce"`
		Email     string      `json:"email"`
	}
	decode := func(part string, dst any) error {
		b, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, dst)
	}
	if decode(parts[0], &header) != nil || header.Algorithm != "RS256" || header.KeyID == "" || decode(parts[1], &claims) != nil {
		return identity{}, errors.New("invalid JWT claims")
	}
	key, err := signingKey(ctx, client, header.KeyID)
	if err != nil {
		return identity{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return identity{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return identity{}, err
	}
	now := time.Now().Unix()
	if claims.Issuer != "https://auth.openai.com" || claims.Subject == "" || claims.Nonce != nonce || claims.Expires <= now || claims.NotBefore > now+60 || claims.IssuedAt > now+60 || !hasAudience(claims.Audience, clientID) {
		return identity{}, errors.New("JWT issuer, audience, expiry or nonce mismatch")
	}
	return identity{Subject: claims.Subject, Email: claims.Email}, nil
}

// signingKey 从 OpenAI 发布的 JWKS 中按 kid 查找 RSA 公钥，用于验证 ID Token 签名。
func signingKey(ctx context.Context, client *http.Client, kid string) (*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("cannot load OpenAI signing keys")
	}
	var document struct {
		Keys []struct {
			KeyType   string `json:"kty"`
			Algorithm string `json:"alg"`
			KeyID     string `json:"kid"`
			Modulus   string `json:"n"`
			Exponent  string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&document); err != nil {
		return nil, err
	}
	for _, item := range document.Keys {
		if item.KeyID != kid || item.KeyType != "RSA" || item.Algorithm != "RS256" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(item.Modulus)
		if err != nil {
			return nil, err
		}
		e, err := base64.RawURLEncoding.DecodeString(item.Exponent)
		if err != nil {
			return nil, err
		}
		exponent := 0
		for _, b := range e {
			exponent = exponent<<8 | int(b)
		}
		if exponent < 3 || len(e) > 4 {
			return nil, errors.New("invalid RSA exponent")
		}
		modulus := new(big.Int).SetBytes(n)
		if modulus.BitLen() < 2048 {
			return nil, errors.New("OpenAI RSA signing key is too small")
		}
		return &rsa.PublicKey{N: modulus, E: exponent}, nil
	}
	return nil, errors.New("OpenAI signing key was not found")
}

// loopbackCallback 用本机 IP 和端口构造精确的 OAuth callback URL，拒绝非回环地址。
func loopbackCallback(host string) (string, error) {
	name, port, err := net.SplitHostPort(host)
	if err != nil || net.ParseIP(name) == nil || !net.ParseIP(name).IsLoopback() || port == "" || port == "0" {
		return "", errors.New("open the workspace at http://127.0.0.1:<port> to use OAuth")
	}
	return "http://127.0.0.1:" + port + callbackPath, nil
}

// isLoopback 判断 HTTP 请求的远端地址是否来自本机回环网卡。
func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	return err == nil && net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

// sameOrigin 拒绝由其他网站发起的浏览器写请求，降低跨站请求伪造风险。
func sameOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && origin.Scheme == "http" && origin.Host == r.Host && origin.User == nil
}

// randomValue 生成指定字节数的密码学随机值，并编码成 URL 安全字符串。
func randomValue(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hasScope 按完整单词查找空格分隔的授权 scope，避免部分字符串误匹配。
func hasScope(scopes, want string) bool {
	for _, scope := range strings.Fields(scopes) {
		if scope == want {
			return true
		}
	}
	return false
}

// hasAudience 兼容 OIDC 中 audience 为单字符串或字符串数组的两种 JSON 表示。
func hasAudience(audience any, want string) bool {
	switch value := audience.(type) {
	case string:
		return value == want
	case []any:
		for _, item := range value {
			if item == want {
				return true
			}
		}
	}
	return false
}

// writeJSON 为本机 OAuth 控制端点写出禁止缓存的 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
