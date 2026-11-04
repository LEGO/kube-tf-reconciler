FROM golang:1.27.3-alpine3.24 AS build

RUN apk add --no-cache ca-certificates && update-ca-certificates

WORKDIR /src

# Download modules in their own layer. Only a go.mod or go.sum change runs this again.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# Declare the args late: a new value invalidates the cache for each RUN after the ARG.
ARG SHA
ARG DATE

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -ldflags "-X cmd.commit=$SHA -X cmd.date=$DATE" -o krec main.go

FROM alpine:3.24 AS krec

# Upgrade base packages and require patched brotli-libs (CVE-2026-111497, pulled in via git -> libcurl).
# The version constraint makes the build fail instead of silently shipping a vulnerable image.
RUN apk upgrade --no-cache && \
    apk add --no-cache ca-certificates git openssh-client 'brotli-libs>=1.2.1-r0'

COPY --from=build /src/krec /usr/local/bin/krec
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["krec", "operator"]
