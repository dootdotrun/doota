# Templates, static assets, and migrations are all embedded in the binary, so the
# runtime image needs nothing but the binary and CA certificates.

FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first so they cache independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary: no cgo, no dynamic loader needed at runtime.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/doot ./cmd/doot

FROM alpine:3.21

# TLS roots for Neon, the model API, Sprites, and GitHub.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 doot

COPY --from=build /out/doot /usr/local/bin/doot

USER doot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/doot"]
