# Media Hub (`hub/`)

Read `hub/hub.md` first (every file, endpoint and setting), then `docs/ROADMAP.md`. `hub/README.md` is
what someone installing the add-on reads: keep it true when the options or endpoints change.

Things a newcomer trips on:

- The add-on's own build compiles the hub and runs its tests (`Dockerfile`): a failing test or a
  compile error leaves the old version running, and the error is in the add-on's Log in Home Assistant
  (Settings, Add-ons, Media Hub), not in the deploy script's output. Look there after every deploy.
- The code is Linux-only (one `syscall` in `memory.go`), so vet it as Linux whatever the machine:
  `GOOS=linux go vet ./...`. `GOOS=linux go test ./...` builds but can't run off Linux; run the tests
  on a Linux machine, in Docker, or in the add-on's build.
- Bump `version` in `config.yaml` for every deploy; the same version is only rebuilt, and the script
  says "use Update instead of Rebuild" when they differ (harmless).
- Both apps use the hub: the TV app the antenna relay, logins and updates; the Music app the library
  and getting music. Say which app a hub change is for in the commit.
- The TorBox and NZBgeek keys are the add-on's options, typed in by whoever runs the hub: never print
  them, never send them to the apps, `redact` them in anything logged. Read them through the
  supervisor API if a test needs them, without echoing them.
- Write Go tests as files (heredocs mangle backslashes) and keep files' line endings as they are.
- Deploy only when asked: a deploy restarts the add-on for everyone using it.
- Streams are four separate cogs (finder, sorter, record keeper, feedback; hub.md "The stream system:
  four cogs"). Fit every change to one cog and keep them talking only through their interfaces.

## Phone notices (`publicip.go`)

The same rule in all three of the owner's Pi add-ons (Media Hub, Home watch, Home upkeep):

1. Every notice is also left in Home Assistant's notifications:
   `POST /core/api/services/persistent_notification/create` with `SUPERVISOR_TOKEN`
   (`homeassistant_api: true` in `config.yaml`).
2. Then it goes to every phone: each `mobile_app_*` notify service in `GET /core/api/services`,
   `POST /core/api/services/notify/<name>` with title, message and `data`, once per phone.
3. Quiet: while `/share/phone-quiet-until.txt` holds an ISO time with a zone still to come, step
   2 is skipped (and logged); step 1 still happens. Missing file, bad text, or a time without a
   zone means not quiet. The file needs `share:rw` in `config.yaml`.
4. A failure to notify is logged, never crashes the add-on.
