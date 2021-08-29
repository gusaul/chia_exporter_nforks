FROM golang:alpine AS builder

WORKDIR /build
COPY . /build/chia_exporter_nforks
RUN apk add --update --no-cache --virtual build-dependencies \
 && cd chia_exporter_nforks \
 && go build -tags netgo

FROM alpine
COPY --from=builder /build/chia_exporter_nforks/chia_exporter_nforks /usr/bin/chia_exporter_nforks

EXPOSE 9133

CMD /usr/bin/chia_exporter_nforks
