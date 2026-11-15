# Media Hub

A Home Assistant add-on that serves the Media Hub apps on your own network: live TV from an antenna,
and a music library, with no cloud account in between. The apps are the
[TV app and the Music app](https://github.com/ExpertLizard56/app-updates), which install and update
themselves from the hub. This add-on's source is
[github.com/ExpertLizard56/media-hub](https://github.com/ExpertLizard56/media-hub).

## What it does

- **Pairs an app with an install code.** No hub is built into the apps: each device you own is paired
  with yours. The add-on's page shows an address and an install code; the app's "Connect to a Media
  Hub" takes both. Each app that pairs gets a key of its own, which can be blocked or removed on its
  own, and learns the hub's address on the internet if you set one.
- **Hands out and updates the apps.** The newest TV app can be installed from the hub's address (so a
  new box, stick or phone needs no downloader), and a paired app checks the hub for a newer version and
  updates itself.
- **Keeps the apps' logs.** Each device sends its event log (start-ups, playback health, network,
  crashes, and so on) to the hub, kept for 30 days per device and downloadable whole from the add-on's
  page. The hub's own log (tuners, signal, music, connections) sits beside it.
- **Relays antenna channels.** With an HDHomeRun tuner on your network, the hub hands its lineup and
  guide to the TV app and passes the channels through, one tuner shared by everyone watching the same
  channel. Nothing is transcoded.
- **Serves a music library.** Every MP3, FLAC, WAV, M4A, AAC, OGG and Opus file you put in
  `/share/media-hub/music` becomes a library the Music app can browse and play, with covers, waveforms
  for its progress bar and smaller copies for phones away from home.
- **Gets albums, if you want it to.** Optional: with your own TorBox and NZBgeek keys, the hub looks an
  album up on MusicBrainz, finds it on Usenet and puts it in the music folder, tagged.

Nothing leaves your network except what you ask for: MusicBrainz lookups, and the album fetches with
your keys. The keys stay on the hub and are never sent to an app.

## Installing

[![Open your Home Assistant instance and show the add app repository dialog with a specific repository URL pre-filled.](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2FExpertLizard56%2Fmedia-hub)

1. Home Assistant, **Settings → Add-ons → Add-on Store**.
2. The three dots at the top right → **Repositories**: add
   `https://github.com/ExpertLizard56/media-hub` and close.
3. **Media Hub** now appears in the store (under the new repository); install it. It runs on a
   Raspberry Pi or any 64-bit ARM or x86-64 Home Assistant machine.
4. **Start** it: it listens on port 8765, speaks both plain HTTP and its own TLS on that port, and
   needs the host network so it can find the HDHomeRun by broadcast.
5. **Configuration**: fill in the options below (all are optional), save, and restart the add-on.
6. Open the add-on's page (**Open web UI**, or **Media Hub** in the sidebar). It shows the address to
   give a device and the install code; in the app, choose "Connect to a Media Hub" and enter both.

The apps themselves are published at
[github.com/ExpertLizard56/app-updates](https://github.com/ExpertLizard56/app-updates): install one on
the device first, or open the hub's address on it (`http://homeassistant.local:8765/`) once an app
version has been put in `/share/media-hub/updates`.

## Options

Set in Home Assistant under **Settings → Add-ons → Media Hub → Configuration**; they're read when the
add-on starts, so restart it after a change.

| Option | What |
| --- | --- |
| `hdhomerun` | The HDHomeRun's address on your network (an IP or a name). Empty finds it automatically with SiliconDust's discovery broadcast, which is what most people want. |
| `remote_address` | The address apps should use when they're away from home, as `name:port` (the name your router's dynamic DNS keeps pointed at your internet address, with the port you forward to the hub). Empty means the hub is used at home only. |
| `https_port` | Optional. A second port that serves the same hub over HTTPS, for a certificate from a real certificate authority (for a reverse proxy, or a browser that needs a certificate it trusts). `0` turns it off. It needs `https_domain` as well. |
| `https_domain` | Optional. The folder of the HTTPS certificate in Home Assistant's `/ssl`: the hub uses `/ssl/<https_domain>/fullchain.pem` and `privkey.pem`. Needed with `https_port`. A certificate another add-on renews is used within a minute, without restarting the hub. |
| `tmdb_api_key` | Optional. A TMDB API key (or read access token) for the movie catalog: rows of movies refreshed daily, which a home-network-only Stremio add-on at `/stremio/manifest.json` shows for testing. Empty turns it off. |
| `torbox_api_key` | Optional. Your TorBox API key (TorBox, Settings, API), for getting albums: TorBox does the Usenet downloading. |
| `nzbgeek_api_key` | Optional. Your NZBgeek API key (your profile there), which searches Usenet for the albums. |

Both keys are needed for album fetching, and both stay on the hub: the apps never see them, and they're
taken out of anything logged or shown.

Phone notices are also left in Home Assistant's own notifications. To stop the phone alerts for a
while, put the time to start again in `/share/phone-quiet-until.txt` as an ISO time with its zone
(`2026-11-16T12:00:00-05:00`): Home Assistant's notification still appears, but no phone is notified
until then. A missing file, bad text, or a time without a zone means phones aren't quiet.

## The port, and where the files go

- **Port 8765**, HTTP and TLS on the same port. At home nothing needs a key; from outside, an app pairs
  first and uses its own key over TLS with the hub's own certificate (made on first start, kept in
  `/share/media-hub/tls`). Only what a new device needs to get in is allowed over plain HTTP.
- **An optional HTTPS port.** With `https_port` and `https_domain` set, the same hub is also served
  over HTTPS on that port, using the certificate in `/ssl/<https_domain>/` (kept up to date by
  whatever add-on renews it; a new certificate is used within a minute). Port 8765 keeps working as
  before, and the same access rules apply on both. The add-on uses the host network, so the new port
  needs nothing else in its configuration.
- **`/share/media-hub/`** holds everything:
  `music/` (your audio files, the only part you put there), `updates/` (the app versions handed out),
  `diagnostics/` (each device's log and the hub's own), `music-mobile/` and `music-waves/` (made from
  your music, rebuilt if lost), `covers/`, plus `access.json`, `devices.json` and `tls/`.
- **Back it up.** Home Assistant's backups only include `share` if you tick that folder in the backup
  settings. Keeping a copy of `tls/` and `access.json` matters most: without them the hub makes new
  keys and a new certificate, and every device has to pair again.
- **Music conversions** (the small copies and waveforms) use ffmpeg at the lowest priority and happen
  once per track, so playing music doesn't keep the machine busy.

## Endpoints, briefly

The hub's own page (Home Assistant's ingress) shows the install code, every device with its logs, and
the hub's log by section. The apps use `/pair/<code>`, `/update/`, `/install`, `/music/...`,
`/diagnostics`, `/share/...` and the `/antenna/...` relay; `hub.md` in this folder lists them all.

## Licence

MIT, see `LICENSE`.

