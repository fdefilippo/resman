FROM golang:1.21-alpine AS builder

WORKDIR /app

# Installa dipendenze necessarie
RUN apk add --no-cache git ca-certificates tzdata

# Copia prima i file di modulo per caching
COPY go.mod go.sum ./
RUN go mod download

# Copia il resto del codice
COPY . .

# Build del binario
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X 'main.version=docker'" -o resman

# ============================================================================
FROM alpine:3.18

# Installa certificati SSL e fuso orario
RUN apk add --no-cache ca-certificates tzdata

# Crea utente non-root per sicurezza
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

# Copia binario e configurazione
COPY --from=builder /app/resman /usr/local/bin/resman
COPY --from=builder /app/config/resman.conf.example /etc/resman.conf

# Configura permessi
RUN mkdir -p /var/log /var/run/resman \
    && chown -R appuser:appgroup /var/log /var/run/resman \
    && chmod 755 /usr/local/bin/resman

USER appuser
WORKDIR /home/appuser

# Esponi porta Prometheus
EXPOSE 9101

# Volume per configurazione esterna
VOLUME ["/etc/resman.conf"]

ENTRYPOINT ["resman"]
CMD ["--config", "/etc/resman.conf"]
