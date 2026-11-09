package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := &client{clientID: "id", secret: "secret", refresh: "refresh"}
	if err := c.loadConfig(); err != nil {
		t.Fatal(err)
	}
	if c.configPath != filepath.Join(home, ".spotify-cli", "config") {
		t.Fatal(c.configPath)
	}
	if err := c.saveConfig(); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{c.configPath, 0600}, {filepath.Dir(c.configPath), 0700}} {
		info, err := os.Stat(item.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != item.mode {
			t.Fatalf("permissions %v", info.Mode())
		}
	}
	loaded := &client{clientID: "env-id"}
	if err := loaded.loadConfig(); err != nil {
		t.Fatal(err)
	}
	if loaded.clientID != "env-id" || loaded.secret != "secret" || loaded.refresh != "refresh" {
		t.Fatal("config precedence failed")
	}
	if err := os.WriteFile(c.configPath, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loaded.loadConfig(); err == nil {
		t.Fatal("accepted corrupt config")
	}
}

func TestCallbackValidation(t *testing.T) {
	results := make(chan authResult, 1)
	h := callbackHandler("expected", results)
	for _, path := range []string{"/callback?state=wrong&code=bad", "/callback?state=expected", "/other?state=expected&code=bad"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code < 400 {
			t.Fatal("accepted invalid callback")
		}
		if len(results) != 0 {
			t.Fatal("invalid callback consumed authorization")
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/callback?state=expected&error=access_denied", nil))
	if (<-results).err == nil {
		t.Fatal("denial ignored")
	}
}

func TestAuthorize(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		id, secret, ok := r.BasicAuth()
		if !ok || id != "id" || secret != "secret" || r.Form.Get("code") != "code" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != redirectURI {
			t.Error("incorrect code exchange")
		}
		io.WriteString(w, `{"access_token":"access","refresh_token":"refresh"}`)
	}))
	defer s.Close()
	c := &client{http: s.Client(), tokenURL: s.URL, clientID: "id", secret: "secret", configPath: filepath.Join(t.TempDir(), ".spotify-cli", "config")}
	browser := func(ctx context.Context, address string) error {
		u, err := url.Parse(address)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("client_id") != "id" || q.Get("redirect_uri") != redirectURI || !strings.Contains(q.Get("scope"), "user-modify-playback-state") || len(q.Get("state")) < 32 {
			t.Error("incorrect authorization URL")
		}
		callback := redirectURI + "?" + url.Values{"state": {q.Get("state")}, "code": {"code"}}.Encode()
		req, err := http.NewRequestWithContext(ctx, "GET", callback, nil)
		if err != nil {
			return err
		}
		resp, err := s.Client().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := c.authorize(context.Background(), io.Discard, browser); err != nil {
		t.Fatal(err)
	}
	var saved credentials
	data, err := os.ReadFile(c.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ClientID != "id" || saved.ClientSecret != "secret" || saved.RefreshToken != "refresh" || c.token != "access" {
		t.Fatal("tokens not saved")
	}
}

func TestRotatedTokenPersisted(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"access_token":"access","refresh_token":"rotated"}`)
	}))
	defer s.Close()
	c := &client{http: s.Client(), tokenURL: s.URL, clientID: "id", secret: "secret", refresh: "old", configPath: filepath.Join(t.TempDir(), "config")}
	if err := c.refreshAccess(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved credentials
	json.Unmarshal(data, &saved)
	if saved.RefreshToken != "rotated" {
		t.Fatal("rotation lost")
	}
}
