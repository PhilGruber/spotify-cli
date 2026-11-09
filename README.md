# Spotify CLI

A non-interactive Go client for searching Spotify and controlling playback on Spotify Connect devices. Playback and search commands never prompt or read stdin. First-run authentication opens a browser when a client ID and secret are available but no refresh token is configured. JSON goes to stdout; errors go to stderr with exit status 1. Built with the Go standard library.

## Build

```sh
go build -o spotify .
```

Use the Go version specified in go.mod or newer.

## Authentication

Create an app in the [Spotify Developer Dashboard](https://developer.spotify.com/dashboard). Register this exact redirect URI in the app settings:

```text
http://127.0.0.1:8888/callback
```

Set the client ID and client secret, then run a command:

```sh
export SPOTIFY_CLIENT_ID='your-app-client-id'
export SPOTIFY_CLIENT_SECRET='your-app-client-secret'
./spotify devices
```

When no refresh token is available, the CLI starts a callback server on `127.0.0.1:8888`, opens Spotify's authorization page in your default browser, and waits up to three minutes for consent. It also prints the authorization URL to stderr so you can open it manually if browser launch fails. The browser must be on the same machine as the CLI (or use port forwarding). Ctrl-C cancels the flow. Port 8888 must be available.

After approval, it exchanges the authorization code, saves all three credentials as JSON in `$HOME/.spotify-cli/config`, and continues the original command. The directory uses permissions `0700`, the file uses `0600`, and writes replace the file atomically. The file contains plaintext secrets; keep it private. Help and invalid commands do not start authorization.

Subsequent runs load the saved credentials automatically; no environment variables are needed. Non-empty `SPOTIFY_CLIENT_ID`, `SPOTIFY_CLIENT_SECRET`, and `SPOTIFY_REFRESH_TOKEN` values override their corresponding saved values. Supplied credentials are saved when an API command first uses them. If Spotify rotates a refresh token, its replacement is saved automatically.

For unattended use, provision the config once before running automation, or supply an existing refresh token:

```sh
export SPOTIFY_CLIENT_ID='your-app-client-id'
export SPOTIFY_CLIENT_SECRET='your-app-client-secret'
export SPOTIFY_REFRESH_TOKEN='your-user-refresh-token'
```

You can also provide a user access token via `SPOTIFY_ACCESS_TOKEN`. If a client ID and secret are configured without a refresh token, browser authorization still runs. Otherwise an access token can be used on its own. PKCE refresh credentials remain supported: omit `SPOTIFY_CLIENT_SECRET` and ensure no secret is saved in the config.

The CLI requests `user-read-playback-state` and `user-modify-playback-state`. Spotify requires consent during initial setup; later commands run unattended. Client Credentials tokens cannot control user playback. Playback requires Spotify Premium and access to the app, including any Development Mode user restrictions. See Spotify's [Authorization Code flow](https://developer.spotify.com/documentation/web-api/tutorials/code-flow).

## Commands

Flags go **before** positional arguments. Quote device names containing spaces.

```sh
# Search songs, albums and playlists (default: 10 results per type)
./spotify search 'Miles Davis'
./spotify search --type track --limit 5 'So What'
./spotify search --type album,artist --offset 10 'Miles Davis'

# Discover available Spotify Connect speakers and other players
./spotify devices

# Transfer to an exact device name (case-insensitive) or device ID
./spotify connect 'Living Room'
./spotify connect --play 'Living Room'

# Play a song, album or playlist on a chosen device
./spotify play --device 'Living Room' spotify:track:0aWMVrwxPNYkKmFthzmpRi
./spotify play --device 'Living Room' spotify:album:1weenld61qoidwYuZ1GESA
./spotify play --device 'Living Room' 'https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M'

# Resume/pause the active player, or specify --device
./spotify play
./spotify pause
```

Search returns Spotify's JSON response, including item URIs, pagination and metadata. Limits are 1–10 per type; offset is 0–1000. Playback accepts Spotify URIs and HTTPS `open.spotify.com` track/album/playlist URLs, including share query parameters and localized URLs. It does not guess a song from a title: search first and pass the chosen URI.

`connect` transfers playback without starting paused playback unless `--play` is supplied. `play --device` directly targets the chosen device. Duplicate device names fail with an error; use an ID to disambiguate. Device IDs may change, so discover them again when needed. Successful playback commands output `{"ok":true}`, meaning Spotify accepted the request.

## Spotify Connect limits

The CLI uses Spotify's [available devices API](https://developer.spotify.com/documentation/web-api/reference/get-a-users-available-devices), not LAN discovery. A speaker must be online, associated with your Spotify account, and exposed by Spotify Connect. Some devices are not returned by the API; restricted devices cannot be controlled. This client cannot provision speakers, wake offline devices, pair Bluetooth hardware, or stream audio itself. Open Spotify on a player if the devices list is empty.

Playback and transfer behavior follows Spotify's [Player API](https://developer.spotify.com/documentation/web-api/reference/start-a-users-playback). Requests have a 30-second HTTP timeout. Rate limits report Spotify's Retry-After delay and exit without retrying playback commands, so automation can decide when to retry.

## Checks

```sh
go test ./...
go vet ./...
```

Tests use local HTTP servers for request payloads, device matching, token refresh, argument validation, rate-limit errors, OAuth callback validation, credential persistence and refresh-token rotation. Real playback requires your credentials and an online device.
