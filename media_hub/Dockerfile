# Built on the Pi by Home Assistant when the add-on is installed or rebuilt (scripts/deploy-hub.sh).
# One static Go binary on plain Alpine: it needs nothing from Home Assistant's base image.
FROM golang:1-alpine AS build
WORKDIR /src
# Home Assistant passes the add-on's version (config.yaml); the hub logs it.
ARG BUILD_VERSION=dev
COPY go.mod *.go ./
# The tests run on every build, so a broken one never gets installed.
RUN CGO_ENABLED=0 go test ./...
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${BUILD_VERSION}" -o /media-hub .

FROM alpine:3
# ffmpeg makes the music's small copies (musicmobile.go); it only runs while converting.
RUN apk add --no-cache tzdata ffmpeg
COPY --from=build /media-hub /usr/bin/media-hub
CMD ["/usr/bin/media-hub"]
