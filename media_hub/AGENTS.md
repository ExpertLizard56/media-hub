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
