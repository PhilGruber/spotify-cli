package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const redirectURI = "http://127.0.0.1:8888/callback"

type credentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

func (c *client) loadConfig() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home directory: %w", err)
	}
	c.configPath = filepath.Join(home, ".spotify-cli", "config")
	data, err := os.ReadFile(c.configPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var saved credentials
	if err = json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	if c.clientID == "" {
		c.clientID = saved.ClientID
	}
	if c.secret == "" {
		c.secret = saved.ClientSecret
	}
	if c.refresh == "" {
		c.refresh = saved.RefreshToken
	}
	return nil
}

func (c *client) saveConfig() error {
	dir := filepath.Dir(c.configPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("secure config directory: %w", err)
	}
	data, err := json.MarshalIndent(credentials{c.clientID, c.secret, c.refresh}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return fmt.Errorf("create config: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), c.configPath); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

func openBrowser(ctx context.Context, address string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", address)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", address)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", address)
	}
	return cmd.Run()
}

type authResult struct {
	code string
	err  error
}

func callbackHandler(state string, results chan<- authResult) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid authorization state", http.StatusBadRequest)
			return
		}
		result := authResult{code: q.Get("code")}
		if q.Get("error") != "" {
			result.err = errors.New("Spotify authorization was denied; run the command again to retry")
		} else if result.code == "" {
			http.Error(w, "Missing authorization code", http.StatusBadRequest)
			return
		}
		select {
		case results <- result:
		default:
			http.Error(w, "Authorization already received", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if result.err != nil {
			io.WriteString(w, "Spotify authorization denied. You can close this window.")
			return
		}
		io.WriteString(w, "Authorization received. You can close this window and return to the terminal.")
	})
}

func (c *client) authorize(parent context.Context, log io.Writer, browser func(context.Context, string) error) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:8888")
	if err != nil {
		return fmt.Errorf("start OAuth callback at %s (register this redirect URI in Spotify): %w", redirectURI, err)
	}
	defer listener.Close()
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	state := base64.RawURLEncoding.EncodeToString(random[:])
	results := make(chan authResult, 1)
	server := &http.Server{Handler: callbackHandler(state, results), ReadHeaderTimeout: 5 * time.Second}
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()
	defer server.Close()
	query := url.Values{"client_id": {c.clientID}, "response_type": {"code"}, "redirect_uri": {redirectURI}, "scope": {"user-read-playback-state user-modify-playback-state"}, "state": {state}}
	address := "https://accounts.spotify.com/authorize?" + query.Encode()
	fmt.Fprintf(log, "Spotify authorization required. Opening browser.\nIf needed, open this URL manually:\n%s\nWaiting up to 3 minutes for approval...\n", address)
	browserCtx, stopBrowser := context.WithTimeout(ctx, 10*time.Second)
	// Browser launch must not delay callback handling or cancellation.
	go func() { defer stopBrowser(); _ = browser(browserCtx, address) }()
	var result authResult
	select {
	case result = <-results:
	case err = <-stopped:
		return fmt.Errorf("OAuth callback server stopped: %w", err)
	case <-ctx.Done():
		return fmt.Errorf("waiting for Spotify authorization: %w", ctx.Err())
	}
	if result.err != nil {
		return result.err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {result.code}, "redirect_uri": {redirectURI}}
	req, err := http.NewRequestWithContext(ctx, "POST", c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.clientID, c.secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("exchange authorization code: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("authorization code exchange failed (HTTP %d); check client credentials and registered redirect URI", resp.StatusCode)
	}
	var tokens struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokens); err != nil {
		return fmt.Errorf("decode authorization tokens: %w", err)
	}
	if tokens.Access == "" || tokens.Refresh == "" {
		return errors.New("authorization response is missing access_token or refresh_token")
	}
	c.token, c.refresh = tokens.Access, tokens.Refresh
	if err = c.saveConfig(); err != nil {
		return err
	}
	fmt.Fprintln(log, "Spotify credentials saved to", c.configPath)
	return nil
}
