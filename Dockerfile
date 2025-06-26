FROM golang:1.24-alpine AS build

RUN apk update && apk upgrade && apk add --no-cache ca-certificates
RUN update-ca-certificates

ARG SHA
ARG DATE

COPY . /src
WORKDIR /src

RUN CGO_ENABLED=0 go build -ldflags "-X cmd.commit=$SHA -X cmd.date=$DATE" -o krec main.go

FROM alpine:3.19 AS krec

RUN apk add --no-cache ca-certificates git openssh-client

COPY --from=build /src/krec /usr/local/bin/krec
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENTRYPOINT ["krec", "operator"]