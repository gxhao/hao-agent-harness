package openaioauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Protocol reference: https://github.com/earendil-works/pi/blob/main/packages/ai/src/auth/oauth/openai-codex.ts
const (
	codexClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexTokenURL    = "https://auth.openai.com/oauth/token"
	codexRedirectURI = "http://localhost:1455/auth/callback"
)

// NewCodex loads Codex credentials without requiring a running API server.
func NewCodex(path string, client *http.Client) (*Manager, error) {
	if path == "" || client == nil {
		return nil, errors.New("credential path and HTTP client are required")
	}
	privateClient := *client
	privateClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	m := &Manager{path: path, name: "hao-agent-harness", client: &privateClient, codex: true}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, errors.New("cannot read OpenAI Codex credential file")
	}
	if json.Unmarshal(b, &m.token) != nil || m.token.ClientID != codexClientID {
		return nil, errors.New("OpenAI Codex credential file is invalid")
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, errors.New("cannot protect OpenAI Codex credential file")
	}
	return m, nil
}

// LoginOptions lets the CLI supply browser launch and manual callback input.
type LoginOptions struct {
	Output      io.Writer
	OpenBrowser func(string) error
	ManualInput <-chan string
}

// Login uses PKCE and a temporary loopback listener; a pasted URL also works when port 1455 is occupied.
func (m *Manager) Login(ctx context.Context, options LoginOptions) error {
	if !m.codex {
		return errors.New("CLI login requires the openai_codex provider")
	}
	if options.Output == nil {
		options.Output = io.Discard
	}
	state, err := randomValue(32)
	if err != nil {
		return err
	}
	verifier, err := randomValue(32)
	if err != nil {
		return err
	}
	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"client_id": {codexClientID}, "response_type": {"code"},
		"redirect_uri": {codexRedirectURI}, "scope": {"openid profile email offline_access"},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"}, "id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow": {"true"}, "originator": {m.name},
	}
	authorization := "https://auth.openai.com/oauth/authorize?" + query.Encode()
	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	listener, listenErr := net.Listen("tcp", "127.0.0.1:1455")
	if listenErr == nil {
		srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
		srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if r.URL.Path != "/auth/callback" {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			code, err := parseCodexCallback(r.URL.Query(), state)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				// An unrelated tab must not cancel the current login attempt.
				if r.URL.Query().Get("state") != state {
					return
				}
			}
			select {
			case results <- result{code, err}:
			default:
			}
			if err == nil {
				io.WriteString(w, "Authorization received. Return to your terminal to finish login.")
			}
		})
		served := make(chan struct{})
		go func() {
			defer close(served)
			if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				select {
				case results <- result{err: errors.New("OAuth callback listener failed")}:
				default:
				}
			}
		}()
		defer func() { srv.Close(); <-served }()
	} else {
		fmt.Fprintln(options.Output, "Port 1455 is unavailable; paste the full callback URL after browser login.")
		if options.ManualInput == nil {
			return errors.New("port 1455 is unavailable and manual input is disabled")
		}
	}
	fmt.Fprintln(options.Output, "OpenAI Codex 登录地址：\n"+authorization)
	if options.OpenBrowser != nil {
		if err := options.OpenBrowser(authorization); err != nil {
			fmt.Fprintln(options.Output, "无法自动打开浏览器，请手动打开上面的地址。")
		}
	}
	fmt.Fprintln(options.Output, "等待浏览器授权；也可粘贴完整回调 URL 后按 Enter（Ctrl+C 取消）：")
	manual := options.ManualInput
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case input, ok := <-manual:
			if !ok {
				manual = nil
				if listenErr != nil {
					return errors.New("manual callback input closed")
				}
				continue
			}
			u, err := url.Parse(strings.TrimSpace(input))
			if err != nil {
				fmt.Fprintln(options.Output, "回调 URL 无效，请重新粘贴。")
				continue
			}
			code, err := parseCodexCallback(u.Query(), state)
			if err != nil {
				if u.Query().Get("state") == state && u.Query().Get("error") != "" {
					return err
				}
				fmt.Fprintln(options.Output, err.Error())
				continue
			}
			return m.exchangeCodex(ctx, code, verifier)
		case result := <-results:
			if result.err != nil {
				return result.err
			}
			return m.exchangeCodex(ctx, result.code, verifier)
		}
	}
}

func parseCodexCallback(query url.Values, state string) (string, error) {
	if query.Get("state") != state {
		return "", errors.New("OAuth state mismatch; paste the full callback URL for this login")
	}
	if query.Get("error") != "" {
		return "", errors.New("OpenAI authorization was not completed")
	}
	if query.Get("code") == "" {
		return "", errors.New("OAuth callback is missing the authorization code")
	}
	return query.Get("code"), nil
}

func (m *Manager) exchangeCodex(ctx context.Context, code, verifier string) error {
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {codexClientID},
		"code": {code}, "code_verifier": {verifier}, "redirect_uri": {codexRedirectURI}}
	var reply tokenReply
	if err := m.tokenRequest(ctx, form, &reply); err != nil {
		return err
	}
	next, err := codexCredentials(reply)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.token
	m.token = next
	if err := m.saveLocked(); err != nil {
		m.token = previous
		return errors.New("could not save OpenAI Codex credentials")
	}
	return nil
}

// Like pi, account metadata is decoded from the access token returned by the TLS token endpoint.
// This is not a general JWT signature verifier and is never used to authorize incoming requests.
func codexCredentials(reply tokenReply) (credentials, error) {
	if reply.AccessToken == "" || reply.RefreshToken == "" || reply.ExpiresIn <= 0 || reply.ExpiresIn > 365*24*60*60 ||
		(reply.TokenType != "" && !strings.EqualFold(reply.TokenType, "Bearer")) {
		return credentials{}, errors.New("OpenAI Codex returned invalid credentials")
	}
	parts := strings.Split(reply.AccessToken, ".")
	if len(parts) != 3 {
		return credentials{}, errors.New("OpenAI Codex access token is invalid")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err != nil || json.Unmarshal(b, &claims) != nil || strings.TrimSpace(claims.Auth.AccountID) == "" || strings.ContainsAny(claims.Auth.AccountID, "\r\n") {
		return credentials{}, errors.New("OpenAI Codex token is missing a valid account ID")
	}
	return credentials{ClientID: codexClientID, AccountID: claims.Auth.AccountID,
		AccessToken: reply.AccessToken, RefreshToken: reply.RefreshToken, IDToken: reply.IDToken,
		ExpiresAt: time.Now().Add(time.Duration(reply.ExpiresIn) * time.Second)}, nil
}

func (m *Manager) codexAccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token.AccessToken == "" || m.token.RefreshToken == "" {
		return "", errors.New("run server --login openai first")
	}
	if time.Until(m.token.ExpiresAt) > 90*time.Second {
		return m.token.AccessToken, nil
	}
	var reply tokenReply
	if err := m.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {codexClientID}, "refresh_token": {m.token.RefreshToken}}, &reply); err != nil {
		return "", errors.New("OpenAI Codex refresh failed; run server --login openai again")
	}
	next, err := codexCredentials(reply)
	if err != nil {
		return "", err
	}
	if next.AccountID != m.token.AccountID {
		return "", errors.New("OpenAI Codex refresh changed the account")
	}
	m.token = next
	if err := m.saveLocked(); err != nil {
		return "", errors.New("could not save refreshed OpenAI Codex credentials")
	}
	return m.token.AccessToken, nil
}

func (m *Manager) AccountID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.token.AccountID
}
