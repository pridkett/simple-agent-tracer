FROM node:22-alpine AS frontend
WORKDIR /src/frontend
COPY frontend/package*.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.26-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY assets.go frontend_dev.go frontend_embed.go openapi.json ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY --from=frontend /src/frontend/dist/ ./frontend/dist/
RUN CGO_ENABLED=0 go build -trimpath -tags frontend -ldflags="-s -w" -o /out/tracer ./cmd/tracer
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM alpine:3.23 AS certificates
RUN apk add --no-cache ca-certificates

FROM scratch
COPY --from=backend /out/tracer /tracer
COPY --from=backend --chown=65532:65532 /out/data/ /data/
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENV TRACER_DATABASE_URL=/data/tracer.db
EXPOSE 4318
ENTRYPOINT ["/tracer"]
