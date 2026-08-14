FROM artifactory.novus.legogroup.io/cgr/go:1.26@sha256:f2fd006bcf534fb35df5c2f7595f0dda395a2c7a7daa99d986f045d52e563d35 AS build

ARG SHA
ARG DATE

COPY . /src
WORKDIR /src

RUN CGO_ENABLED=0 go build -ldflags "-X cmd.commit=$SHA -X cmd.date=$DATE" -o krec main.go

FROM artifactory.novus.legogroup.io/cgr/chainguard-base:v20230214@sha256:4c0a58ebbfacbd8c8248d2d2098d5219e4f1c64237c0ed5147759fe9c0206432 AS krec

RUN apk add --no-cache git openssh-client

COPY --from=build /src/krec /usr/local/bin/krec
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["krec", "operator"]