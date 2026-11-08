# scrobbler

Scrobbles what you play in Apple Music on your Mac to [teal.fm](https://teal.fm).

It checks Music every 5 seconds through `osascript`. Once you've listened to enough of a track, it writes an `fm.teal.feed.play` record to your AT Protocol repository. The threshold follows teal's play lexicon: the whole track if it's under 2 minutes, otherwise half of it, capped at 4 minutes. It also updates your teal "now playing" status. It's a single Go binary with no dependencies.

- Only time spent actually playing counts. Pausing, seeking forward and leaving Music paused don't add to it.
- Repeat-one plays and replays of the same track count as separate plays.
- If a write fails because you're offline or the PDS is down, the play goes into a queue (`queue.json`) and is retried every minute. Each play gets a record key built from its start time, so a retry can't create a duplicate.
- If Music isn't running, it stays closed. The tool never launches it.

## Setup

You need Go 1.24 or newer (`brew install go`).

```sh
git clone https://github.com/dathagerty/scrobbler && cd scrobbler
go build -o ~/.local/bin/scrobbler .   # or anywhere on your PATH

scrobbler init      # asks for your handle; stores an app password in the Keychain
scrobbler check     # logs in and shows what Music is playing; writes nothing
scrobbler install   # LaunchAgent: runs in the background, starts at login
```

Use an **app password** (Bluesky: Settings → Privacy and security → App passwords), not your main password.

The first time it runs, macOS asks whether `scrobbler` (or `osascript`) may control Music. Allow it. If you missed the prompt, go to System Settings → Privacy & Security → Automation.

Files live in `~/Library/Application Support/teal-scrobbler/`:

| file | |
|---|---|
| `config.json` | settings (below) |
| `queue.json` | plays waiting to be sent |
| `scrobbler.log` | log output when it runs as a LaunchAgent |

To try it without writing anything, run `scrobbler -dry-run run`. To remove the LaunchAgent, run `scrobbler uninstall`. If you rebuild the binary at the same path, restart the agent with `launchctl kickstart -k gui/$(id -u)/com.dathagerty.teal-scrobbler`.

## Config

```json
{
  "handle": "you.bsky.social",
  "pds": "",
  "collection": "fm.teal.feed.play",
  "update_status": true,
  "poll_seconds": 5
}
```

- `pds`: leave this empty to resolve your PDS from your handle.
- `collection`: teal moved from `fm.teal.alpha.feed.play` to `fm.teal.feed.play`, and its indexer reads both. Set it to the alpha name if you want your plays to match older clients. The status collection follows the same choice.
- The password is looked up in this order: the `TEAL_APP_PASSWORD` env var, `app_password` in the config, then the Keychain item `teal-scrobbler` / your handle.

## What gets sent

Each play includes the track name, the artist (Music's artist string, as one entry), the album, the duration, the time playback began, `musicServiceUri: https://music.apple.com`, and a client agent string. It doesn't include MusicBrainz IDs or ISRC, because Music's scripting interface doesn't expose them. Teal's indexer does its own matching.

Music's scripting interface only gives a single artist string, so a track by "A & B" goes in as one artist, `A & B`. It isn't split: splitting on `&` or `,` would break band names like "Simon & Garfunkel".

## Scrobbling from other devices

This tool only sees playback in the Music app on the Mac it runs on. It can't see your iPhone, HomePod, CarPlay or the web player. Apple doesn't offer a feed of play events for those:

- Apple Music API, `GET /v1/me/recent/played/tracks`: needs a paid Apple Developer membership for the developer token and a MusicKit sign-in for the user token. It returns roughly your last 30 tracks, **without timestamps**, with repeats removed. You can't reliably rebuild plays from that.
- iOS's on-device library keeps play counts and last-played dates for library items. An iOS app can read them, and that's how existing iOS scrobblers (Last.fm ones) work. Building one means an iOS app, and the data is still coarse: one last-played date per track, not every play.

The practical route is two steps: use an iOS app that scrobbles Apple Music to Last.fm, then bridge Last.fm to teal. `user.getRecentTracks` has real timestamps. A bridge would fit easily as a second mode of this tool. You'd want to turn off Mac scrobbling to Last.fm at the same time, or every Mac play would show up twice.
