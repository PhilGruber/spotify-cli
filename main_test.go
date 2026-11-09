package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCommands(t *testing.T) {
	for _, tc := range []struct {
		args                     []string
		method, path, key, value string
	}{
		{[]string{"search", "--type", "album", "Miles Davis"}, "GET", "/search", "q", "Miles Davis"},
		{[]string{"devices"}, "GET", "/me/player/devices", "", ""},
		{[]string{"play", "spotify:track:0123456789012345678901"}, "PUT", "/me/player/play", "uris", "spotify:track:0123456789012345678901"},
		{[]string{"play", "https://open.spotify.com/album/0123456789012345678901?si=test"}, "PUT", "/me/player/play", "context_uri", "spotify:album:0123456789012345678901"},
		{[]string{"play", "spotify:playlist:0123456789012345678901"}, "PUT", "/me/player/play", "context_uri", "spotify:playlist:0123456789012345678901"},
		{[]string{"play"}, "PUT", "/me/player/play", "", ""},
		{[]string{"pause"}, "PUT", "/me/player/pause", "", ""},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != tc.method || r.URL.Path != tc.path {
					t.Errorf("request %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer test" {
					t.Error("missing token")
				}
				if tc.method == "GET" {
					if tc.key != "" && r.URL.Query().Get(tc.key) != tc.value {
						t.Error("wrong query")
					}
					io.WriteString(w, `{"items":[]}`)
					return
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if tc.key != "" {
					var value string
					if tc.key == "uris" {
						var values []string
						json.Unmarshal(body[tc.key], &values)
						if len(values) == 1 {
							value = values[0]
						}
					} else {
						json.Unmarshal(body[tc.key], &value)
					}
					if value != tc.value {
						t.Errorf("body %s", body)
					}
				}
				w.WriteHeader(204)
			}))
			defer s.Close()
			c := &client{http: s.Client(), base: s.URL, token: "test"}
			var out bytes.Buffer
			if err := run(context.Background(), tc.args, &out, io.Discard, c); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Errorf("calls %d", calls)
			}
			if !json.Valid(out.Bytes()) {
				t.Errorf("invalid output %s", out.String())
			}
		})
	}
}

func TestDeviceSelection(t *testing.T) {
	for _, tc := range []struct {
		selector string
		fail     bool
	}{{"id1", false}, {"office", false}, {"Duplicate", true}, {"Restricted", true}, {"missing", true}} {
		t.Run(tc.selector, func(t *testing.T) {
			transfers := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					io.WriteString(w, `{"devices":[{"id":"id1","name":"Office"},{"id":"id2","name":"Duplicate"},{"id":"id3","name":"Duplicate"},{"id":"id4","name":"Restricted","is_restricted":true}]}`)
					return
				}
				transfers++
				var b struct {
					IDs  []string `json:"device_ids"`
					Play bool     `json:"play"`
				}
				json.NewDecoder(r.Body).Decode(&b)
				if r.URL.Path != "/me/player" || len(b.IDs) != 1 || b.IDs[0] != "id1" || !b.Play {
					t.Errorf("bad transfer %+v", b)
				}
				w.WriteHeader(204)
			}))
			defer s.Close()
			c := &client{http: s.Client(), base: s.URL, token: "test"}
			err := run(context.Background(), []string{"connect", "--play", tc.selector}, io.Discard, io.Discard, c)
			if (err != nil) != tc.fail {
				t.Fatalf("error %v", err)
			}
			if tc.fail && transfers != 0 {
				t.Error("transferred after invalid selection")
			}
		})
	}
}

func TestRefreshAndRetry(t *testing.T) {
	apiCalls, refreshCalls := 0, 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshCalls++
			r.ParseForm()
			id, secret, ok := r.BasicAuth()
			if !ok || id != "client" || secret != "secret" || r.Form.Get("refresh_token") != "refresh" {
				t.Error("bad refresh credentials")
			}
			io.WriteString(w, `{"access_token":"new"}`)
			return
		}
		apiCalls++
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer new" {
			t.Error("bad refreshed authorization")
		}
		io.WriteString(w, `{"devices":[]}`)
	}))
	defer s.Close()
	c := &client{http: s.Client(), base: s.URL, tokenURL: s.URL + "/token", token: "old", clientID: "client", secret: "secret", refresh: "refresh"}
	if _, err := c.request(context.Background(), "GET", "/me/player/devices", nil); err != nil {
		t.Fatal(err)
	}
	if apiCalls != 2 || refreshCalls != 1 {
		t.Errorf("calls api=%d refresh=%d", apiCalls, refreshCalls)
	}
}

func TestValidationBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{{"search"}, {"search", "--limit", "11", "x"}, {"search", "--type", "bad", "x"}, {"play", "bad"}, {"play", "spotify:artist:0123456789012345678901"}, {"devices", "extra"}, {"connect"}, {"pause", "extra"}} {
		if err := run(context.Background(), args, io.Discard, io.Discard, &client{}); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestRateLimit(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"message":"Too many requests"}}`)
	}))
	defer s.Close()
	c := &client{http: s.Client(), base: s.URL, token: "secret-token"}
	_, err := c.request(context.Background(), "GET", "/search", nil)
	if err == nil || !strings.Contains(err.Error(), "12 seconds") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error %v", err)
	}
}
