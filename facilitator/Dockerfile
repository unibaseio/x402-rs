# Build stage
FROM golang:1.24-alpine AS build
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/facilitator .

# Runtime stage
FROM alpine:3.20
# TLS roots for HTTPS RPC endpoints; wget ships with busybox for the healthcheck.
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 x402
USER x402

COPY --from=build /out/facilitator /usr/local/bin/facilitator

EXPOSE 4022
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s \
  CMD wget -qO- http://127.0.0.1:4022/supported >/dev/null || exit 1

ENTRYPOINT ["facilitator"]
