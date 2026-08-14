# syntax=docker/dockerfile:1
#
# The attendant is a member of a room, not a service anything calls: it opens no
# port and serves nothing. So the image is the binary and a CA bundle — the
# rooms, the model and the voice are all reached over TLS, and that is the whole
# of what it needs from a filesystem.
#
# Pure Go, no cgo: the Opus decoder is pion's, written in Go, so nothing here
# links against libopus and the binary runs on a scratch-thin base.
FROM golang:1.26.5-alpine AS build
WORKDIR /src
ENV GOPROXY=https://proxy.golang.org,direct
COPY go.mod go.sum ./
RUN --mount=type=cache,id=attendant-gomod,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,id=attendant-gomod,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /attendant .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
    && addgroup -S hanzo && adduser -S hanzo -G hanzo
COPY --from=build /attendant /usr/local/bin/attendant
USER hanzo
ENTRYPOINT ["/usr/local/bin/attendant"]
