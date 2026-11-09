package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"
)

const usage = `spotify: non-interactive Spotify controller (JSON output)

Commands (flags must precede positional arguments):
  search [--type track,album,playlist] [--limit 10] [--offset 0] QUERY
  devices
  connect [--play] DEVICE_ID_OR_EXACT_NAME
  play [--device DEVICE_ID_OR_EXACT_NAME] [SPOTIFY_URI_OR_URL]
  pause [--device DEVICE_ID_OR_EXACT_NAME]

play accepts tracks, albums and playlists; omit the URI to resume.
Environment:
  SPOTIFY_ACCESS_TOKEN                      user access token
  SPOTIFY_CLIENT_ID, SPOTIFY_REFRESH_TOKEN   refresh credentials
  SPOTIFY_CLIENT_SECRET                    optional for PKCE clients
Credentials are loaded from ~/.spotify-cli/config; environment values override them.
With a client ID and secret but no refresh token, first-run browser login starts.
Register http://127.0.0.1:8888/callback as your app redirect URI.
Required scopes: user-read-playback-state user-modify-playback-state
Playback requires Spotify Premium. See README.md for authentication.
`

type client struct {
	http                                             *http.Client
	base, tokenURL, token, clientID, secret, refresh string
	configPath                                       string
	configReady                                      bool
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	c := &client{http: &http.Client{Timeout: 30 * time.Second}, base: "https://api.spotify.com/v1", tokenURL: "https://accounts.spotify.com/api/token", token: os.Getenv("SPOTIFY_ACCESS_TOKEN"), clientID: os.Getenv("SPOTIFY_CLIENT_ID"), secret: os.Getenv("SPOTIFY_CLIENT_SECRET"), refresh: os.Getenv("SPOTIFY_REFRESH_TOKEN")}
	err := c.loadConfig()
	if err == nil {
		err = run(ctx, os.Args[1:], os.Stdout, os.Stderr, c)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "spotify:", err)
		os.Exit(1)
	}
}

func (c *client) refreshAccess(ctx context.Context) error {
	if c.clientID == "" || c.refresh == "" {
		return errors.New("set SPOTIFY_ACCESS_TOKEN or SPOTIFY_CLIENT_ID and SPOTIFY_REFRESH_TOKEN")
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.refresh}}
	if c.secret == "" {
		form.Set("client_id", c.clientID)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.secret != "" {
		req.SetBasicAuth(c.clientID, c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("refresh token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("token refresh failed (HTTP %d); check refresh credentials", resp.StatusCode)
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode token: %w", err)
	}
	if result.AccessToken == "" {
		return errors.New("token response has no access_token")
	}
	c.token = result.AccessToken
	if result.RefreshToken != "" && result.RefreshToken != c.refresh {
		c.refresh = result.RefreshToken
		if c.configPath != "" {
			return c.saveConfig()
		}
	}
	return nil
}

func (c *client) request(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	if c.configPath != "" && !c.configReady {
		if c.refresh == "" && c.clientID != "" && c.secret != "" {
			if err := c.authorize(ctx, os.Stderr, openBrowser); err != nil {
				return nil, err
			}
		} else if c.clientID != "" && c.refresh != "" {
			if err := c.saveConfig(); err != nil {
				return nil, err
			}
		}
		c.configReady = true
	}
	if c.token == "" {
		if err := c.refreshAccess(ctx); err != nil {
			return nil, err
		}
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode == 401 && attempt == 0 && c.refresh != "" {
			if err := c.refreshAccess(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			var detail struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(data, &detail)
			message := detail.Error.Message
			switch resp.StatusCode {
			case 401:
				message += "; access token expired or invalid"
			case 403:
				message += "; check Premium subscription, OAuth scopes and app access"
			case 404:
				message += "; ensure a Spotify Connect device is online"
			case 429:
				message += "; retry after " + resp.Header.Get("Retry-After") + " seconds"
			}
			return nil, fmt.Errorf("Spotify HTTP %d: %s", resp.StatusCode, message)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return json.RawMessage(`{"ok":true}`), nil
		}
		if !json.Valid(data) {
			return nil, errors.New("Spotify returned invalid JSON")
		}
		return data, nil
	}
	return nil, errors.New("authentication failed")
}

type device struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Restricted bool   `json:"is_restricted"`
}

func (c *client) resolveDevice(ctx context.Context, selector string) (string, error) {
	data, err := c.request(ctx, "GET", "/me/player/devices", nil)
	if err != nil {
		return "", err
	}
	var list struct {
		Devices []device `json:"devices"`
	}
	if err = json.Unmarshal(data, &list); err != nil {
		return "", err
	}
	var matches []device
	for _, d := range list.Devices {
		if d.ID == selector && d.ID != "" {
			if d.Restricted {
				return "", errors.New("device is restricted and cannot be controlled")
			}
			return d.ID, nil
		}
	}
	for _, d := range list.Devices {
		if strings.EqualFold(d.Name, selector) {
			matches = append(matches, d)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("device %q not available; run spotify devices", selector)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("device name %q is ambiguous; use its ID", selector)
	}
	if matches[0].ID == "" || matches[0].Restricted {
		return "", errors.New("device cannot be controlled")
	}
	return matches[0].ID, nil
}

var spotifyID = regexp.MustCompile(`^[A-Za-z0-9]{22}$`)

func mediaURI(input string) (string, string, error) {
	if strings.HasPrefix(input, "https://open.spotify.com/") {
		u, err := url.Parse(input)
		if err != nil {
			return "", "", err
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 3 && strings.HasPrefix(parts[0], "intl-") {
			parts = parts[1:]
		}
		if len(parts) != 2 {
			return "", "", errors.New("expected a Spotify track, album or playlist URL")
		}
		input = "spotify:" + parts[0] + ":" + parts[1]
	}
	parts := strings.Split(input, ":")
	if len(parts) != 3 || parts[0] != "spotify" || !spotifyID.MatchString(parts[2]) {
		return "", "", errors.New("expected spotify:track:ID, spotify:album:ID, spotify:playlist:ID or an open.spotify.com URL")
	}
	switch parts[1] {
	case "track", "album", "playlist":
		return input, parts[1], nil
	}
	return "", "", errors.New("only tracks, albums and playlists can be played")
}

func run(ctx context.Context, args []string, out, errOut io.Writer, c *client) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, usage)
		return err
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(errOut)
	var kind, selector string
	var limit, offset int
	var start bool
	switch cmd {
	case "search":
		fs.StringVar(&kind, "type", "track,album,playlist", "comma-separated search types")
		fs.IntVar(&limit, "limit", 10, "results per type (1-10)")
		fs.IntVar(&offset, "offset", 0, "result offset (0-1000)")
	case "devices":
	case "connect":
		fs.BoolVar(&start, "play", false, "start playback after transfer")
	case "play", "pause":
		fs.StringVar(&selector, "device", "", "device ID or exact name")
	default:
		return fmt.Errorf("unknown command %q; use --help", cmd)
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	var path, method string
	var body any
	switch cmd {
	case "search":
		if fs.NArg() == 0 || strings.TrimSpace(strings.Join(fs.Args(), " ")) == "" {
			return errors.New("search requires a query")
		}
		if limit < 1 || limit > 10 || offset < 0 || offset > 1000 {
			return errors.New("limit must be 1-10 and offset 0-1000")
		}
		seen := map[string]bool{}
		for _, t := range strings.Split(kind, ",") {
			if (t != "track" && t != "album" && t != "playlist" && t != "artist") || seen[t] {
				return errors.New("search types must be unique: track, album, playlist, artist")
			}
			seen[t] = true
		}
		q := url.Values{"q": {strings.Join(fs.Args(), " ")}, "type": {kind}, "limit": {fmt.Sprint(limit)}, "offset": {fmt.Sprint(offset)}}
		method, path = "GET", "/search?"+q.Encode()
	case "devices":
		if fs.NArg() != 0 {
			return errors.New("devices takes no arguments")
		}
		method, path = "GET", "/me/player/devices"
	case "connect":
		if fs.NArg() != 1 {
			return errors.New("connect requires one device ID or quoted name")
		}
		id, err := c.resolveDevice(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		method, path = "PUT", "/me/player"
		body = map[string]any{"device_ids": []string{id}, "play": start}
	case "play", "pause":
		if (cmd == "pause" && fs.NArg() != 0) || fs.NArg() > 1 {
			return errors.New("play accepts one URI; pause takes no arguments (flags precede arguments)")
		}
		method, path = "PUT", "/me/player/"+cmd
		body = map[string]any{}
		if cmd == "play" && fs.NArg() == 1 {
			uri, t, err := mediaURI(fs.Arg(0))
			if err != nil {
				return err
			}
			if t == "track" {
				body = map[string]any{"uris": []string{uri}}
			} else {
				body = map[string]any{"context_uri": uri}
			}
		}
		if selector != "" {
			id, err := c.resolveDevice(ctx, selector)
			if err != nil {
				return err
			}
			path += "?" + url.Values{"device_id": {id}}.Encode()
		}
	}
	data, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err = json.Indent(&pretty, data, "", "  "); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, pretty.String())
	return err
}
