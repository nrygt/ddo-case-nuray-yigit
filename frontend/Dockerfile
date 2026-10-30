# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/web .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/web /app/web
USER nonroot:nonroot
EXPOSE 3000
ENTRYPOINT ["/app/web"]
