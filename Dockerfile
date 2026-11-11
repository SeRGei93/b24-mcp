# syntax=docker/dockerfile:1.7
FROM golang:1.26-alpine@sha256:30b1e546a9c7fa3b747c2bdba3a0c7bdf3a02ebb4c5b8405d2f948d7e8aac4ec AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN mkdir -p /out/data /out/tmp \
  && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/b24 ./cmd/b24

FROM scratch AS runtime
WORKDIR /app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=1000:1000 /out/tmp /tmp
COPY --from=build --chown=1000:1000 /out/data /app/data
COPY --from=build --chown=1000:1000 /out/b24 /app/b24
USER 1000:1000
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/app/b24", "-healthcheck"]
ENTRYPOINT ["/app/b24"]
