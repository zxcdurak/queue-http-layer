# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/api ./cmd/api

FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 app

COPY --from=build /out/api /usr/local/bin/api

USER app

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/api"]
