FROM golang:1.26-alpine AS base-image

RUN apk --no-cache --no-progress add git ca-certificates && update-ca-certificates

FROM scratch AS base

COPY --from=base-image /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY dist/staraudit-linux-amd64 /staraudit

ENTRYPOINT ["/staraudit"]