FROM golang:alpine as build
MAINTAINER timo.taskinen@vincit.fi
LABEL maintainer "greg@burd.me"
ENV VERSION=0.0.8
ENV LOGSPOUT_VERSION=3.2.11
RUN mkdir -p /go/src
WORKDIR /go/src
VOLUME /mnt/routes
EXPOSE 80

RUN apk --no-cache add curl git gcc musl-dev

RUN curl -fSL -o logspout.tar.gz "https://github.com/gliderlabs/logspout/archive/v${LOGSPOUT_VERSION}.tar.gz" \
    && tar -zxvf logspout.tar.gz \
    && rm logspout.tar.gz \
    && mkdir -p /go/src/github.com/gliderlabs/ \
    && mv logspout-${LOGSPOUT_VERSION} /go/src/github.com/gliderlabs/logspout

WORKDIR /go/src/github.com/gliderlabs/logspout
RUN echo 'import ( _ "github.com/blockfi/logspout-humio" )' >> /go/src/github.com/gliderlabs/logspout/modules.go
RUN go get -d -v github.com/blockfi/logspout-humio@v${VERSION}
RUN go get -d -v ./...
RUN go build -v -ldflags "-X main.Version=$(cat VERSION)" -o ./bin/logspout


FROM alpine:latest
COPY --from=build /go/src/github.com/gliderlabs/logspout/bin/logspout /go/bin/logspout
ENTRYPOINT ["/go/bin/logspout"]
