FROM --platform=$BUILDPLATFORM artifactory.novus.legogroup.io/cgr/go:1.27.4@sha256:011ef78e704a8bec9fbc43c46071e7b798c42d7bd9e7ee78ab61bf46708dd259 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Declare the args late: a new value invalidates the cache for each RUN after the ARG.
# TARGETOS/TARGETARCH are set by BuildKit for each requested --platform. The compiler
# always runs natively on $BUILDPLATFORM and cross-compiles, so no emulation is needed here.
ARG TARGETOS
ARG TARGETARCH
ARG SHA
ARG DATE

# The compiler cache mount helps local builds only. CI starts with an empty mount.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags "-X cmd.commit=$SHA -X cmd.date=$DATE" -o krec main.go

FROM artifactory.novus.legogroup.io/cgr/git-base@sha256:5c6cfb71874cad8eb32f963f4c0e711bd3bbed3f7ae68b60e87345b1de4e0fc7 AS krec
COPY --from=build /src/krec /usr/local/bin/krec
ENTRYPOINT ["krec", "operator"]
