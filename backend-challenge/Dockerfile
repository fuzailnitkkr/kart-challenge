# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY data ./data

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w' -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

ARG DESCRIPTION="Build a mini food ordering app"
LABEL org.opencontainers.image.description="${DESCRIPTION}"

COPY --from=builder /out/api /app/api
COPY --from=builder /src/data /app/data

ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["/app/api"]
