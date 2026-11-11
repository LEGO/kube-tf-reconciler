FROM artifactory.novus.legogroup.io/cgr/go:1.27.3@sha256:97e7726d4e7d3b077694a2c8eb44f5c8a77ce1a66aefa8653e228a99a2011102 AS build

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

FROM artifactory.novus.legogroup.io/cgr/chainguard-base:latest@sha256:213aad08918741464ce15c6e9a1254ccb3926e0187dd20314132108c6837efd5 AS krec

RUN apk add --no-cache git openssh-client

COPY --from=build /src/krec /usr/local/bin/krec
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["krec", "operator"]
