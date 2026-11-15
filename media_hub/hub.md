# The Media Hub (`hub/`, Go, a Home Assistant add-on)

Current state, not history: when you change how something works, change its section here in the same
commit. The apps' side of updates, the log upload and reaching the hub is `hubclient`
(`shared/platform.md`). What the hub is for, for someone installing it, is `hub/README.md`.

`hub/` is a small Go service that runs as a Home Assistant add-on (slug `media_hub`, `config.yaml` in
this folder). `Dockerfile` builds it as one static binary on Alpine, for aarch64 and amd64. It keeps
its files in `/share/media-hub` and serves the apps on port 8765. Each app is paired with a hub on its
own device (shared `HubSettings`, Away from home): no hub is in a build, and local.properties holds
only the SDK path and the signing key.

Contents: [Files and settings](#files-and-settings) · [Updates](#updates) · [Diagnostics](#diagnostics) ·
[Away from home](#away-from-home) · [The music library](#the-music-library) ·
[Getting new music](#getting-new-music) · [Logins and the internet address](#logins-and-the-internet-address) ·
[The antenna relay](#the-antenna-relay)

## Files and settings

| File | What |
| --- | --- |
| `main.go` | routes, updates (`/update/`, `/install`), diagnostics upload, the add-on's options |
| `access.go` | who gets in: the home network, or the hub key over the hub's own TLS (Away from home) |
| `https.go` | the optional HTTPS port (`https_port`, `https_domain`): the same handler with a certificate from `/ssl`, read again when it is renewed |
| `eventlog.go`, `memory.go` | the hub's own log (`diagnostics/hub/`), its memory every 15 min |
| `music.go`, `musictags.go`, `musicmobile.go`, `musicwave.go` | the music library |
| `musicbrainz.go`, `musicmatch.go`, `musicfetch.go` | getting new music |
| `share.go`, `publicip.go` | handing a login between devices; the home's internet address |
| `antenna.go`, `discover.go`, `monitor.go`, `tsstats.go` | the antenna relay (the TV app's) |
| `catalog.go`, `catalogdata/seed.json` | the movie catalog (rows from TMDB, daily lineup); `/catalog/rows` (home only) shows today's rows |
| `catalogsources.go` | the catalog's facts from outside TMDB: IMDb ratings (daily data file) and Wikidata (Oscars, based on a book or a true story, film series) |
| `catalogrecord.go` | one record per catalog movie with everything known about it (catalog, IMDb, Wikidata, stream check, the PC's plays and link tests), for other processes: `/catalog/movie/<id>`, `/catalog/movies` (home only) |
| `stremio.go` | a home-only Stremio add-on over the catalog, for testing it (`/stremio/manifest.json`) |

Settings are the add-on's options in Home Assistant (Settings, Add-ons, Media Hub, Configuration;
`/data/options.json`, read at start: restart the add-on after a change): `hdhomerun` (the tuner's
address on the home network; empty = find it by broadcast), `remote_address` (the hub's address from
the internet, handed to apps as they pair; empty = the hub is used at home only), `torbox_api_key` and
`nzbgeek_api_key` (only for getting new music; they never leave the hub),
`https_port` and `https_domain` (an optional second port serving the same hub over HTTPS with the
certificate in `/ssl/<https_domain>/`; both must be set, and 0 = off), `tmdb_api_key` (the movie
catalog; empty = off). Tests
(`*_test.go`) run in the add-on's Docker build, so a failing test stops a deploy and the old version
keeps running; the error is in the add-on's Log (Settings, Add-ons, Media Hub). `go test ./...` and
`go vet ./...` also run in this folder with Go on the machine (`GOOS=linux`: the hub is Linux-only).

`scripts/deploy-hub.sh` copies `hub/` to the Home Assistant machine over SSH and has Home Assistant
build and restart it; bump `version` in `hub/config.yaml` for each change.

## Updates

 The app updates itself (`shared/hubclient/.../update/Updater.kt`, shared with the Music app) from the hub.

- It checks 10 s after start-up, each time the app comes back (if the last look was over 5 minutes ago), and twice a day; a look that fails is tried again every minute, five times. On a phone the first update asks once for "Install unknown apps"; updates keep the login, uninstalling erases it.
- It downloads a newer version and installs it when the app is left.
- The first update on a box needs a person, once: allow TV to install apps, and confirm.
- Test devices: `updates/developers.json` on the hub (`["id", …]`, the device's folder name in `diagnostics/`, one per app) lists devices that look every 30 s while the app is open and put a new version in at once (`AppUpkeep`).
- An app put on with adb can't update itself without asking (Android names no installer): reinstall it once as its own, `adb install -r -i app.tv <apk>` (`-i app.music` for Music). After that it's silent.
- A new box's first install: in the Downloader app, `homeassistant.local:8765` (Home Assistant's own name on the home network; always the newest APK). A name rather than an address, so the app stays findable when the address changes.
- `scripts/publish-update.sh` builds the committed code and publishes it to the app's GitHub release (`ExpertLizard56/app-updates`), which is where apps update from. `/update/` serves what's in `updates/`, for devices that look there.
- The version is the commit count, so publish committed code only.
- `scripts/install.sh` installs as the app (`-i app.tv`), so the box stays able to update itself.
- Every build is signed with the permanent key named in local.properties. Android refuses an update signed with any other key, so losing it means reinstalling on every box by hand. Keep a backup.

## Diagnostics

 Every device sends its event log to the hub (`hubclient` `diagnostics/DiagnosticsUploader.kt`, shared with the Music app).

- It sends 1 min after start-up, every 30 min, and when the app is left.
- Only lines not already sent go.
- They're kept per device and day (UTC, as the log is) in `/share/media-hub/diagnostics/<device id>/` for 30 days, with `box.json` naming the device and its app version.
- Browse them at `/diagnostics/` on the hub (its page in Home Assistant lists them too), or over SSH.
- What a device logs besides its own events: `device_info`, `display` and `audio_output` at start-up;
  `network` (the route traffic takes, the VPN on a device with Tailscale) and `network_link` (the
  Wi-Fi under it: band, link speed, signal) when they change; `previous_exit` (why the app last
  ended, including freezes and low-memory kills); `crash` with its stack; `memory_trim`;
  `app_foreground`/`app_background`; `playback_health` every minute while watching (data rate,
  buffer, fps, dropped frames, audio underruns) and `channel_session` when a channel ends.
- The hub logs its own side in `diagnostics/hub/` (`hub/eventlog.go`): the HDHomeRun's details
  (`hdhomerun_device`), each tuner's signal every minute while in use (`tuner_status`), lost or
  corrupt packets as they happen with the signal then (`stream_errors`), each tuning and viewer
  (`tuned`, `viewer_joined`, `viewer_left`, `tuner_released` with the totals), lineup and guide
  loads, and every hour nobody's watching a `signal_survey` of each station, to follow reception
  over days. `/antenna/status.json` shows the tuners and streams now.

## Away from home

Pairing at home without the code: while /share/media-hub/pair-home-until.txt holds a time to come (RFC 3339), an app on the home network with no key yet asks /pair/home at start and is paired as if it had the install code (access.go). Write the file, open each app once, delete it; devices paired meanwhile stay paired.

(`hub/access.go`, `shared/hubclient/.../app/hub/Hub.kt`). Devices at other houses reach the hub through
one port forwarded on the home router to the hub's port, by name: the router's dynamic DNS keeps that
name pointed at the home's internet address.

- The port speaks plain HTTP and TLS. From the home network (and VPNs into it) everything works as
  before. From anywhere else: TLS with the hub key (`X-Hub-Key`) for everything, except a new
  device's first install at `/i/<install code>` (plain HTTP, or HTTPS on https_port); browsing diagnostics stays home-only.
- The TLS certificate is the hub's own (`/share/media-hub/tls`, 30 years); the app trusts it by its
  fingerprint alone. No domain, certificate authority or sign-up.
- The key and install code are in `/share/media-hub/access.json`; each app keeps what it paired with
  (key, pin, remote address) on its own device, never in the repo. Losing `/share/media-hub` means the
  hub makes new keys and a new certificate, so every device pairs with it again. Keep a copy of `tls/`
  and `access.json` somewhere safe: to restore, copy them back into `/share/media-hub/` and restart the
  hub add-on. Home Assistant's automatic backups only include `share` if that folder is ticked in its
  backup settings.
- Since Nov 14 the internet address is `https_port` with a real certificate
  (`pi.hensleynetwork.freemyip.com:8766`; at home AdGuard points the name at the Pi), so the apps use
  it at home too. `/health` tells them where they are: `X-Hub-Where: home|away`, and to home requests
  `X-Hub-Remote` (the hub's internet address now), which a paired app saves, so a change of
  `remote_address` reaches every device by itself. The app asks the internet address first and uses
  it when it answers, away only when the hub says so (`hub_route` in the log; the Music app streams
  the small copies away); if it doesn't answer, the saved home address (port 8765 on the home network).
  Antenna streams from home never use the upload.
- If the home's internet address changes, the router's dynamic DNS follows it, so devices need
  nothing. The hub still checks every 15 min, logs it and notifies: always a notification in Home
  Assistant itself, and every phone (`mobile_app_*` notify services) unless
  `/share/phone-quiet-until.txt` holds a time still to come (`publicip.go`, `public_address` in the
  hub log). The app pins the hub's certificate, not a name or address, so the name can point anywhere
  without a rebuild.
- A remote device's first install: `<remote address>/i/<install code>` in Downloader.
- An app pairing: `/pair/<install code>` (plain HTTP or TLS, home or away) answers
  `{"key", "pin", "remoteUrl"}`, so the apps carry no hub built in (shared `HubSettings`). `remoteUrl`
  is the add-on option `remote_address`; empty means the hub is home-only.
- Each app that pairs gets a key of its own (`devices.go`, `/share/media-hub/devices.json`), so one
  device can be blocked or removed without the others; devices from before use the shared key in
  access.json. Blocking works from outside the home only (at home no key is asked for).
- The hub's page in Home Assistant (`admin.go`, ingress, the add-on's Open web UI; answers only Home
  Assistant's proxy 172.30.32.2): connect a device (address, install code, New code), every device with
  rename / block / remove; Logs: Device logs (tick All devices or some, then how far back: today, 3, 7,
  14 days or all, counted from local midnight by each line's own time since day files are UTC days; one
  device a .log, several a zip), the hub's log by section (antenna, music library, music downloads,
  connections, hub) with each section's last entry, and everything as one zip. Every downloaded line's
  time is in local time ("2026-11-09 8:02:15 PM"; stored as UTC). Logs download whole, never by line
  counts (the add-on's Log tab is Home Assistant's own).

## The music library

`music.go`: every audio file under `/share/media-hub/music` (MP3, FLAC, WAV, M4A, AAC, OGG, Opus),
looked at again every 5 minutes (only changed files are read again; one scan at a time).

- What a song is: its own tags first (`musictags.go`: ID3v2 in MP3 and WAV, WAV's LIST/INFO, FLAC's
  Vorbis comments, lengths, where the embedded cover is; only the start of a file is read), then its
  folders and name: `Artist/Album/03 - Title`, `Artist - Album/Title`, or just `Title`. A leading number
  is a track number only with a separator or a leading zero ("21 Guns" and "100 Ways to Hate" are
  titles). An `album.json` beside the songs (left by a fetch, below) beats the tags for album, artist,
  year and, by track number, the title.
- Covers: embedded, else `cover`/`folder`/`front`.jpg|png in the folder.
- Small copies (`musicmobile.go`): a track over 320 kbps (FLAC, plain WAV) is converted once, in
  the background at nice 19, to 192 kbps AAC in `music-mobile/<id>.m4a`; a copy that isn't at least
  25% smaller is thrown away (ADPCM WAVs came out bigger). Phones away from home stream these and
  downloads save them. Converting once, not on every play as other music servers do, keeps the hub
  idle while music plays.
- Waveforms (`musicwave.go`): 160 RMS loudness bytes per track (ffmpeg, 8 kHz mono, nice 19) in
  `music-waves/<id>.wave`, made once; `wave` in the library is when (unix seconds), so phones replace
  kept copies when it changes.
- After making copies and waveforms the hub drops those files from Linux's cache (`dropCache`) and
  hands its unused memory back (`releaseMemory`). Its own memory is logged every 15 minutes
  (`memory.go`, `hub_memory`: Go's heap and system memory, the container's anon and file memory), so
  "how lean is it" is measured rather than guessed.

Endpoints the Music app calls:

| Endpoint | What |
| --- | --- |
| `GET /music/library.json` | every track (id, title, artist, album artist, album, track, disc, year, length, format, bytes, hasArt, mobileBytes, wave, added); gzipped once per scan |
| `GET /music/track/{id}` | the file, with Range; `?quality=mobile` for its small copy |
| `GET /music/art/{id}`, `GET /music/wave/{id}` | its cover; its waveform |
| `GET /music/install` | the newest Music app, for a phone's first install |
| `GET /music/lookup?q=` | albums for what was typed (below) |
| `GET /music/album/{id}` | one album: tracks (with `have`), its Usenet copies best first, the fetch if one's on |
| `GET /music/cover/{id}` | an album's cover, 250 px |
| `POST /music/fetch?id=&album=` | get that copy of that album |
| `GET /music/fetches` | what's being got, and what was got in the last month |

## Getting new music

Optional, for anyone with the two keys: albums are identified first (MusicBrainz), then searched on
Usenet through an indexer (NZBgeek) and downloaded by TorBox (its Usenet downloader), so things can be
cross-searched.

- `musicbrainz.go`: albums as MusicBrainz knows them (one request a second at most, "busy" answers
  asked again up to 3 times, answers kept a day, a plain "MediaHub/1" name, no contact details
  sent). A search always asks both "is it an artist?" and "which albums have these words?", and
  `rankLookup` orders them, each with a heading (`group`) the app shows: "Top result", a well-known
  album with exactly the typed name ("dookie" is also a little known artist's name); then the artist's
  whole discography (up to 300, a page of 100 a second): Albums and EPs newest first, Live albums,
  Compilations and Other releases (demos, soundtracks…) most rated on MusicBrainz first, since a
  prolific band can have a hundred live albums; then "More" (or "Albums" when nothing came before), at
  most 25: by how many typed words they cover, real albums before live/demo, then how many editions
  (how well known). Each album says its `kind` (Live, Compilation, Demo…; empty for a studio album).
  Singles and tributes, lullabies, karaoke and the like are left out unless such a word was typed.
  Albums the library has are marked `inLibrary`. An album's track list is its most usual edition (the
  track count most official editions have; digital before CD; earliest). Covers come from the Cover
  Art Archive and are kept in `covers/` (a missing one is remembered for a week).
- `musicmatch.go`: NZBgeek takes only plain words, so the hub decides which results are the album: the
  name must hold the artist's and album's words; karaoke, tribute, covers, lullaby, live, remix, demo
  and the like are out unless the album is one; the size must fit the album's length (about 96 kbps at
  least, 45 MB a minute at most). Best first: CD-quality FLAC, then hi-res FLAC or vinyl rips (hi-res
  by name or by size, over 14 MB a minute), MP3 320, other MP3.
- `musicfetch.go`: Get sends the chosen copy to TorBox, with up to 3 other matching copies behind it.
  TorBox's progress is polled every 15 s (6 hours at most). Finished, the audio and pictures are copied
  into a hidden `music/.incoming/` folder (each file checked against TorBox's size, 3 tries), then
  checked: no audio at all, or fewer songs than the album's track list, fails that copy, and the next
  one is tried by itself. A good one gets `album.json` (and the cover if it had none) and is moved to
  `music/<Artist>/<Album (Year)>/`; the download is deleted from TorBox and the library rescanned.
  Fetches are kept in `fetches.json` (with the NZB links, which hold the indexer key: they're never
  sent to the apps) and carry on after a restart. Keys are taken out of anything logged or shown.
- TorBox's own search isn't used (its search API doesn't answer), and its cache didn't match NZBgeek
  links in testing, so there's no "already on TorBox" check. NZBgeek is asked with `extended=1` so
  download counts (grabs) help rank copies.

## Logins and the internet address

- `share.go`: one device hands its TV provider login to another. The device that has it locks it with a
  short code it shows on screen and `PUT`s it at `/share/<SHA-256 of the code>`; the other device,
  given the code, `GET`s it once. Held in memory only, 10 minutes; the hub never sees the code.
- `publicip.go`: the home's internet address, checked every 15 minutes; a change is logged
  (`public_address`) and sent as a notification through Home Assistant (its mobile app).

## The antenna relay

The TV app's antenna channels come through the hub (`antenna.go`): `/antenna/lineup.json`,
`/antenna/guide.json` (SiliconDust's free guide, paged), `/antenna/stream/{number}` (MPEG-TS, one tuner
shared by every device on the same channel, nothing transcoded) and `/antenna/status.json`. The
HDHomeRun is found by broadcast (`discover.go`) unless the `hdhomerun` option names it; `monitor.go`
and `tsstats.go` log its signal and lost packets (Diagnostics, above). The app's side is `tv/tv.md`.

## The stream system: four cogs

Getting a movie from "it's in a row" to "it plays" is split into four cogs. Each one does one job, talks
to the others only through its published interface (HTTP/JSON, or files in the catalog folder), and can
be replaced without touching the other three. Build every change to fit one cog; if it needs two, it's
two changes.

| Cog | Job | Today | Later | Interface |
|---|---|---|---|---|
| 1. Finder | look up every file there is for a title: indexes, size, hash, resolution, where found, cached on TorBox or not. No ranking, no taste filters | Comet, Meteor, MediaFusion, StremThru Torz (public servers, called by AIOStreams) | Camron's own finder, its own add-on and repo, replacing all four | Stremio stream protocol: `/stream/movie/<id>.json`, every candidate with its facts |
| 2. Sorter | take the finders' candidates, drop duplicates and unwanted ones, rank what's left, hand Stremio / the TV app the list | AIOStreams (Pi add-on, local_aiostreams) | own sorter ranking by what actually works on these boxes; may live in the hub, but as its own file with one entry point (candidates + record in, ordered list out) | Stremio stream protocol out; reads records from cog 3 |
| 3. Record keeper | keep everything known about each title in one record; the only place it's stored | the hub: `catalog.go`, `catalogsources.go`, `catalogrecord.go` | same, growing new sections | `/catalog/movie/<id>`, `/catalog/movies[?since=]`; files in /share/media-hub/catalog/ |
| 4. Feedback | watch what really happens and report it: plays, link tests, stream checks | PC scripts in Home Network\Services\Playback report (Playback report, Test links, Check catalog streams, Copy to Pi) | same, plus whatever the boxes report themselves | writes plays.jsonl, links.jsonl, streams.json into the catalog folder; reads cog 3 |

Rules: the finder never ranks and the sorter never searches; nothing but the record keeper stores
facts about a title (the others read them from it); feedback reports and never changes rankings
itself; record fields are only added, never renamed or removed, so every cog keeps working while
another changes.

## The movie catalog

`catalog.go`, on when `tmdb_api_key` is set. Once a day (on the first start, then after 4am) it pulls
popular English movies per TMDB genre, the last 120 days' releases and the best-rated, plus details
(IMDb id, runtime) of new movies and of any not refreshed in 60 days. Each movie gets tags: the hand
sorting and holiday lists in `catalogdata/seed.json` (embedded), plus TMDB's genres. Files in
`/share/media-hub/catalog/`: `movies.json`, `shown.json` (what each row showed first, for pushing
those down for a week), `state.json`, and `corrections.json`, written by hand: `{"<tmdb id>":
["Tag", ...]}` replaces that movie's tags, read again within a minute of a change. Rows: the holiday
whose dates hold today (the shortest when two do), New Releases, then every tag with 8 or more movies.
After the refresh, `catalogsources.go` reads IMDb's daily ratings file (the rating rows sort by when it
has more votes than TMDB's, and the one Stremio shows) and asks Wikidata about each movie every 30 days,
150 a query: Oscar wins and nominations, based on a book, based on actual events, film series. Those give
the tags Oscar Winners, Based on a Book and Based on a True Story. No keys needed. `catalogrecord.go` puts everything known about each movie into one record for other processes (AI tagging, a stream sorter): the catalog's own data, the stream check (streams.json) and the PC's Playback report plays and link tests (plays.jsonl, links.jsonl, copied into the catalog folder by "Copy to Pi.ps1"), at `/catalog/movie/<tmdb or tt id>` and `/catalog/movies` (`?since=<time>` for only the records updated since), home only. Each source is one section with nothing repeated from another; plays and link tests come summed up (counts, medians), plays newest first, link tests grouped by file with the working, fastest ones first; the plays' offered lists and box hardware stay on the PC. Each row's order is fixed for the day. The TV app will read the rows later; until then `stremio.go`
serves them to Stremio on the home network only (guard turns away everything else without a hub key),
at `https://<https_domain>:<https_port>/stremio/manifest.json`. It is never published.
