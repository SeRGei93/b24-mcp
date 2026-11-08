# syntax=docker/dockerfile:1.7
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/b24 ./cmd/b24

FROM alpine:3.21 AS runtime
RUN apk add --no-cache ca-certificates tzdata \
  && addgroup -g 1000 -S app \
  && adduser -u 1000 -S -G app app \
  && mkdir -p /app/data \
  && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/b24 /app/b24
COPY README.md ./
USER app
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${PORT:-3000}/health" >/dev/null || exit 1
CMD ["/app/b24"]
