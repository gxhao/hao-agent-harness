package openaioauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hao-agent-harness/internal/openaioauth"
)

func codexJWT(account string) string {
	b, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account}})
	return "header." + base64.RawURLEncoding.EncodeToString(b) + ".signature"
}

func TestCodexManualLoginPersistsAndRefreshesOnce(t *testing.T) {
	// Reserve the fixed callback port to exercise pi's manual-input fallback.
	if occupied, err := net.Listen("tcp", "127.0.0.1:1455"); err == nil {
		defer occupied.Close()
	}
	manual := make(chan string, 2)
	var state, challenge string
	var refreshes int
	access := codexJWT("account-one")
	client := &http.Client{Transport: oauthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://auth.openai.com/oauth/token" {
			return nil, fmt.Errorf("unexpected endpoint")
		}
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		if r.Form.Get("client_id") != "app_EMoamEEZ73f0CkXaXp7hrann" || r.Form.Get("resource") != "" {
			return nil, fmt.Errorf("incorrect Codex client or resource")
		}
		expires := 1
		refresh := "refresh-one"
		if r.Form.Get("grant_type") == "authorization_code" {
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(digest[:]) != challenge || r.Form.Get("code") != "test-code" || r.Form.Get("redirect_uri") != "http://localhost:1455/auth/callback" {
				return nil, fmt.Errorf("PKCE exchange mismatch")
			}
		} else {
			refreshes++
			expires = 3600
			refresh = "refresh-two"
			if r.Form.Get("refresh_token") != "refresh-one" {
				return nil, fmt.Errorf("incorrect refresh token")
			}
		}
		b, _ := json.Marshal(map[string]any{"access_token": access, "refresh_token": refresh, "expires_in": expires})
		return oauthResponse(r, "application/json", b), nil
	})}
	path := filepath.Join(t.TempDir(), "nested", "codex.json")
	manager, err := openaioauth.NewCodex(path, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = manager.Login(ctx, openaioauth.LoginOptions{ManualInput: manual, OpenBrowser: func(raw string) error {
		u, _ := url.Parse(raw)
		q := u.Query()
		state, challenge = q.Get("state"), q.Get("code_challenge")
		if u.Path != "/oauth/authorize" || state == "" || q.Get("code_challenge_method") != "S256" || q.Get("codex_cli_simplified_flow") != "true" || q.Get("scope") != "openid profile email offline_access" {
			return fmt.Errorf("incorrect authorization URL")
		}
		manual <- "http://localhost:1455/auth/callback?code=wrong&state=wrong"
		manual <- "http://localhost:1455/auth/callback?code=test-code&state=" + url.QueryEscape(state)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := openaioauth.NewCodex(path, client, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AccountID() != "account-one" {
		t.Fatal("account ID not persisted")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			token, err := reloaded.AccessToken(ctx)
			if err != nil || token != access {
				t.Errorf("refresh failed: %v", err)
			}
		})
	}
	wg.Wait()
	if refreshes != 1 {
		t.Fatalf("refresh calls = %d", refreshes)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "refresh-two") {
		t.Fatal("rotated refresh token was not saved")
	}
}

func TestCodexBrowserCallbackRejectsWrongStateThenSucceeds(t *testing.T) {
	// Use the registered fixed port; another local program can occupy it on developer machines.
	probe, err := net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		t.Skip("callback port is in use")
	}
	probe.Close()
	client := &http.Client{Transport: oauthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := json.Marshal(map[string]any{"access_token": codexJWT("account"), "refresh_token": "refresh", "expires_in": 3600})
		return oauthResponse(r, "application/json", b), nil
	})}
	manager, err := openaioauth.NewCodex(filepath.Join(t.TempDir(), "codex.json"), client, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	local := &http.Client{Timeout: time.Second}
	err = manager.Login(ctx, openaioauth.LoginOptions{OpenBrowser: func(raw string) error {
		u, _ := url.Parse(raw)
		for _, attempt := range []struct {
			state  string
			status int
		}{{"wrong", 400}, {u.Query().Get("state"), 200}} {
			r, err := local.Get("http://127.0.0.1:1455/auth/callback?code=test&state=" + url.QueryEscape(attempt.state))
			if err != nil {
				t.Error(err)
				return err
			}
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode != attempt.status {
				t.Errorf("callback status = %d", r.StatusCode)
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Login must release its temporary callback listener.
	probe, err = net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		t.Fatal("callback port was not released")
	}
	probe.Close()
}

func TestCodexCancellationAndInvalidTokenDoNotSaveCredentials(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "codex.json")
		client := &http.Client{Transport: oauthRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			return oauthResponse(r, "application/json", []byte(`{"access_token":"invalid","refresh_token":"secret","expires_in":3600}`)), nil
		})}
		manager, err := openaioauth.NewCodex(path, client, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		manual := make(chan string, 1)
		err = manager.Login(ctx, openaioauth.LoginOptions{ManualInput: manual, OpenBrowser: func(raw string) error {
			if invalid {
				u, _ := url.Parse(raw)
				manual <- "http://localhost:1455/auth/callback?code=x&state=" + u.Query().Get("state")
			}
			return nil
		}})
		cancel()
		if err == nil {
			t.Fatal("expected login failure")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("failed login saved credentials")
		}
	}
}
