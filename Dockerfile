FROM golang:1.23-alpine AS builder

WORKDIR /go/src/github.com/akyriako/typesense-peer-resolver

ADD . .

RUN set -euxo pipefail \
 && go mod download \
 && CGO_ENABLED=0 go build -ldflags "-s -w" -o tspr .

# Run steps
FROM alpine:3

EXPOSE 8080
COPY --from=builder /go/src/github.com/akyriako/typesense-peer-resolver/tspr /opt

RUN mkdir -p /usr/share/typesense

ENTRYPOINT ["/opt/tspr"]
