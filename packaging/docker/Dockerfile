FROM golang:1.25.7-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Copy module files first for dependency caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the remaining source
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X 'main.version=docker'" -o resman

# ============================================================================
FROM alpine:3.18

# Install CA certificates and timezone data
RUN apk add --no-cache ca-certificates tzdata

# Create an unprivileged runtime user
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

# Copy the binary and default configuration
COPY --from=builder /app/resman /usr/local/bin/resman
COPY --from=builder /app/config/resman.conf.example /etc/resman.conf

# Configure runtime permissions
RUN mkdir -p /var/log /var/run/resman \
    && chown -R appuser:appgroup /var/log /var/run/resman \
    && chmod 755 /usr/local/bin/resman

USER appuser
WORKDIR /home/appuser

# Expose the default Prometheus metrics port
EXPOSE 1974

# Allow an external configuration mount
VOLUME ["/etc/resman.conf"]

ENTRYPOINT ["resman"]
CMD ["--config", "/etc/resman.conf"]
