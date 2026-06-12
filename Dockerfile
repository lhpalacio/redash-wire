# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/redash-wire ./cmd/redash-wire

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/redash-wire /usr/local/bin/redash-wire
# Mount your config read-only at /etc/redash-wire/config.yaml, e.g.:
#   docker run --rm -p 15432:15432 \
#     -v $PWD/config.yaml:/etc/redash-wire/config.yaml:ro redash-wire
EXPOSE 15432 13306
ENTRYPOINT ["/usr/local/bin/redash-wire", "-config", "/etc/redash-wire/config.yaml"]
