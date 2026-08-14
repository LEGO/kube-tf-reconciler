FROM artifactory.novus.legogroup.io/cgr/go:1.26.4@sha256:f2fd006bcf534fb35df5c2f7595f0dda395a2c7a7daa99d986f045d52e563d35 AS build

ARG SHA
ARG DATE

COPY . /src
WORKDIR /src

RUN CGO_ENABLED=0 go build -ldflags "-X cmd.commit=$SHA -X cmd.date=$DATE" -o krec main.go

FROM artifactory.novus.legogroup.io/cgr/chainguard-base:latest@sha256:4187092f534afa184684e41fe5554e6c4c93c1d183b7000ab0e0db71a9371957 AS krec

RUN apk add --no-cache git openssh-client

COPY --from=build /src/krec /usr/local/bin/krec
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["krec", "operator"]
