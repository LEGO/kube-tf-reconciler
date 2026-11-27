FROM artifactory.novus.legogroup.io/cgr/go:1.27.4@sha256:92b60833a7a1e6f4d5bc7381a5c2126f215643158f2ba2a64aaeed904de29790 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Declare the args late: a new value invalidates the cache for each RUN after the ARG.
ARG SHA
ARG DATE

# The compiler cache mount helps local builds only. CI starts with an empty mount.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -ldflags "-X cmd.commit=$SHA -X cmd.date=$DATE" -o krec main.go

FROM artifactory.novus.legogroup.io/cgr/chainguard-base:latest@sha256:9879345765f1f0f04b2a703e2a3a444a3ce9d36a72ed1b234629ff7c09bc9e45 AS krec

RUN apk add --no-cache git openssh-client

COPY --from=build /src/krec /usr/local/bin/krec
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["krec", "operator"]
