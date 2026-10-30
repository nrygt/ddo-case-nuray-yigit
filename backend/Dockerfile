# syntax=docker/dockerfile:1
# Tek imaj, birden fazla binary: api, migrate, simulator, verify, loadtest.
# Compose her servis için farklı komut çalıştırır.

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    for cmd in api migrate simulator verify loadtest; do \
      go build -ldflags="-s -w" -o /out/$cmd ./cmd/$cmd || exit 1; \
    done

# distroless/static: kabuk yok, paket yöneticisi yok, nonroot kullanıcı (uid 65532)
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /app/
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/api"]
