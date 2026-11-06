# Imagen de producción de la API Blood & Sand (binario estático, ~15 MB)
# Las migraciones (migrations/*.sql) y la zona horaria (time/tzdata) van embebidas en el binario.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/bloodandsand-api ./cmd/api

FROM alpine:3.22
# Certificados para HTTPS (Transbank, Mercado Pago, Web Push) y zona horaria del sistema
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H bloodandsand
COPY --from=build /out/bloodandsand-api /usr/local/bin/bloodandsand-api
USER bloodandsand
ENV PORT=8088 \
    APP_ENV=production \
    TZ=America/Santiago
EXPOSE 8088
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${PORT}/api/health" || exit 1
ENTRYPOINT ["/usr/local/bin/bloodandsand-api"]
