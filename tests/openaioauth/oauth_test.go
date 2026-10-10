package openaioauth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hao-agent-harness/internal/openaioauth"
)

type oauthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f oauthRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOAuthStartRequiresLocalAuthenticatedOriginAndUsesPKCE(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openai-oauth.json")
	manager, err := openaioauth.New(path, "hao-agent-harness", http.DefaultClient, []string{"client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	handler := manager.Handler()
	request := httptest.NewRequest(http.MethodPost, "/auth/openai/start", nil)
	request.Host = "127.0.0.1:8080"
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Origin", "http://127.0.0.1:8080")
	request.Header.Set("Authorization", "Bearer client-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	authorizationURL, err := url.Parse(result["authorization_url"])
	if err != nil {
		t.Fatal(err)
	}
	query := authorizationURL.Query()
	if authorizationURL.Host != "auth.openai.com" || query.Get("client_id") != "dynamic_agent_client" || query.Get("response_type") != "code" {
		t.Fatalf("unexpected authorization request: %s", authorizationURL)
	}
	if query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" || query.Get("state") == "" || query.Get("nonce") == "" {
		t.Fatalf("PKCE and anti-forgery values are missing: %v", query)
	}
	if query.Get("redirect_uri") != "http://127.0.0.1:8080/auth/openai/callback" || query.Get("agent_name_hint") != "hao-agent-harness" {
		t.Fatalf("unexpected callback or app identity: %v", query)
	}
}

func TestOAuthControlAndCallbackAreLoopbackOnly(t *testing.T) {
	manager, err := openaioauth.New(filepath.Join(t.TempDir(), "tokens.json"), "hao-agent-harness", http.DefaultClient, []string{"client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("control endpoint requires bearer token", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/auth/openai/status", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		manager.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", w.Code)
		}
	})
	t.Run("callback from network is rejected", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/auth/openai/callback?state=x", nil)
		r.RemoteAddr = "192.0.2.9:12345"
		w := httptest.NewRecorder()
		manager.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "only from this computer") {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
	})
}

func TestCredentialFileContainsStableHostIDAndNoTokensBeforeLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "tokens.json")
	_, err := openaioauth.New(path, "hao-agent-harness", http.DefaultClient, []string{"client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := openaioauth.New(path, "hao-agent-harness", http.DefaultClient, []string{"client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]string
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(saved["ext_agent_host_id"], "urn:uuid:") || saved["access_token"] != "" {
		t.Fatalf("unexpected credential record: %v", saved)
	}
	// Reopening the manager must retain the same machine registration identifier.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/auth/openai/start", nil)
	r.Host = "127.0.0.1:8080"
	r.RemoteAddr = "127.0.0.1:23456"
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	r.Header.Set("Authorization", "Bearer client-secret")
	second.Handler().ServeHTTP(w, r)
	var result map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	u, _ := url.Parse(result["authorization_url"])
	if u.Query().Get("ext_agent_host_id") != saved["ext_agent_host_id"] {
		t.Fatal("host ID changed after manager restart")
	}
}

func TestOAuthCallbackVerifiesIdentityAndRefreshesRotatingToken(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kid := "test-signing-key"
	var idToken string
	client := &http.Client{Transport: oauthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://auth.openai.com/.well-known/jwks.json":
			exponent := big.NewInt(int64(privateKey.PublicKey.E)).Bytes()
			data, _ := json.Marshal(map[string]any{"keys": []any{map[string]string{
				"kty": "RSA", "alg": "RS256", "kid": kid,
				"n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(exponent),
			}}})
			return oauthResponse(r, "application/json", data), nil
		case "https://auth.openai.com/api/accounts/oauth/token":
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("grant_type") == "authorization_code" {
				if r.Form.Get("client_id") != "oaiapp_test" || r.Form.Get("code_verifier") == "" {
					return nil, fmt.Errorf("invalid token request")
				}
				body, _ := json.Marshal(map[string]any{"access_token": "access-one", "refresh_token": "refresh-one", "id_token": idToken, "token_type": "Bearer", "expires_in": 1, "scope": "chatgpt.tokens.use.direct resource.invoke offline_access openid profile email"})
				return oauthResponse(r, "application/json", body), nil
			}
			if r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == "refresh-one" {
				body, _ := json.Marshal(map[string]any{"access_token": "access-two", "refresh_token": "refresh-two", "token_type": "Bearer", "expires_in": 3600, "scope": "chatgpt.tokens.use.direct resource.invoke offline_access openid profile email"})
				return oauthResponse(r, "application/json", body), nil
			}
			return nil, fmt.Errorf("unexpected grant type")
		default:
			return nil, fmt.Errorf("unexpected OAuth endpoint: %s", r.URL)
		}
	})}
	path := filepath.Join(t.TempDir(), "credentials.json")
	manager, err := openaioauth.New(path, "hao-agent-harness", client, []string{"client-secret"})
	if err != nil {
		t.Fatal(err)
	}
	start := httptest.NewRequest(http.MethodPost, "/auth/openai/start", nil)
	start.Host = "127.0.0.1:8080"
	start.RemoteAddr = "127.0.0.1:1234"
	start.Header.Set("Origin", "http://127.0.0.1:8080")
	start.Header.Set("Authorization", "Bearer client-secret")
	started := httptest.NewRecorder()
	manager.Handler().ServeHTTP(started, start)
	var startBody map[string]string
	if err := json.Unmarshal(started.Body.Bytes(), &startBody); err != nil {
		t.Fatal(err)
	}
	authorization, _ := url.Parse(startBody["authorization_url"])
	state := authorization.Query().Get("state")
	nonce := authorization.Query().Get("nonce")
	if state == "" || nonce == "" {
		t.Fatal("authorization state or nonce is missing")
	}
	idToken = signIDToken(t, privateKey, kid, nonce)
	callback := httptest.NewRequest(http.MethodGet, "/auth/openai/callback?code=example-code&state="+url.QueryEscape(state)+"&client_id=oaiapp_test", nil)
	callback.RemoteAddr = "127.0.0.1:8080"
	callbackResult := httptest.NewRecorder()
	manager.Handler().ServeHTTP(callbackResult, callback)
	if callbackResult.Code != http.StatusOK {
		t.Fatalf("callback status=%d body=%s", callbackResult.Code, callbackResult.Body.String())
	}
	access, err := manager.AccessToken(context.Background())
	if err != nil || access != "access-two" {
		t.Fatalf("access token=%q err=%v", access, err)
	}
	var stored map[string]any
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &stored) != nil {
		t.Fatalf("cannot load saved credentials: %v", err)
	}
	if stored["refresh_token"] != "refresh-two" || stored["subject"] != "subject-1" {
		t.Fatalf("credentials were not verified and rotated: %v", stored)
	}
}

func oauthResponse(request *http.Request, contentType string, body []byte) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: request}
}

func signIDToken(t *testing.T, key *rsa.PrivateKey, kid, nonce string) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": "https://auth.openai.com", "sub": "subject-1", "aud": "oaiapp_test", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce, "email": "user@example.com"})
	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedClaims := base64.RawURLEncoding.EncodeToString(claims)
	message := encodedHeader + "." + encodedClaims
	digest := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return message + "." + base64.RawURLEncoding.EncodeToString(signature)
}
