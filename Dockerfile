FROM golang:1.10-alpine AS builder
MAINTAINER "Matt Zahorik <matt.zahorik@gmail.com>"

RUN apk update && \
    apk add git build-base curl ca-certificates && \
    curl -fL -s -o /usr/local/bin/dep https://github.com/golang/dep/releases/download/v0.4.1/dep-linux-amd64 && \
    chmod a+x /usr/local/bin/dep && \
    mkdir -p "$GOPATH/src/github.com/mzahorik/f5-ctlr"

ADD . "$GOPATH/src/github.com/mzahorik/f5-ctlr"

RUN cd "$GOPATH/src/github.com/mzahorik/f5-ctlr" && \
    dep ensure -v
RUN cd "$GOPATH/src/github.com/mzahorik/f5-ctlr" && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a --ldflags='-extldflags "-static"' -o /f5-ctlr

FROM busybox:1.28

COPY --from=builder /f5-ctlr /bin/f5-ctlr

ENTRYPOINT ["/bin/f5-ctlr"]
