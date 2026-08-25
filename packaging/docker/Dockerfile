ARG GO_VERSION=1.25.7

FROM docker.io/library/golang:${GO_VERSION}-bookworm AS go-toolchain

FROM docker.io/library/oraclelinux:9 AS builder

COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
WORKDIR /src

RUN dnf install -y ca-certificates gcc git glibc-devel \
    && dnf clean all \
    && rm -rf /var/cache/dnf

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY cgroup ./cgroup
COPY config ./config
COPY database ./database
COPY internal ./internal
COPY logging ./logging
COPY mcp ./mcp
COPY metrics ./metrics
COPY reloader ./reloader
COPY state ./state

RUN CGO_ENABLED=1 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X 'main.version=container'" \
    -o /out/resman .

FROM docker.io/library/oraclelinux:9

RUN dnf install -y ca-certificates sssd-client tzdata \
    && dnf clean all \
    && rm -rf /var/cache/dnf \
    && install -d -m 0700 /etc/resman /etc/resman/tls \
    && install -d -m 0700 /var/lib/resman \
    && install -d -m 0750 /var/log/resman

COPY --from=builder /out/resman /usr/local/bin/resman
COPY config/resman.conf.example /etc/resman/resman.conf

RUN chmod 0755 /usr/local/bin/resman \
    && chmod 0600 /etc/resman/resman.conf \
    && ldd /usr/local/bin/resman | grep -q 'libc\.so'

# The supported runtime requires host PID/cgroup namespaces and privileged
# access. Running as another user would silently lose trustworthy foreign-user
# process identity and cgroup write access.
USER 0
WORKDIR /var/lib/resman

EXPOSE 1974
STOPSIGNAL SIGTERM

ENTRYPOINT ["/usr/local/bin/resman"]
CMD ["--config", "/etc/resman/resman.conf"]
