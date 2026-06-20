FROM golang:1.26-alpine AS builder

WORKDIR /src

# Copy go mod files first for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
ARG VERSION=dev
RUN go build \
    -ldflags="-X go.kenn.io/roborev/internal/version.Version=${VERSION}" \
    -o /roborev ./cmd/roborev

FROM alpine:3.19

# Install dependencies for freeze + tmux screenshots
RUN apk add --no-cache ca-certificates git bash tmux

# Install freeze for SVG screenshots (detect architecture)
RUN ARCH=$(uname -m) && \
    if [ "$ARCH" = "aarch64" ]; then FREEZE_ARCH="arm64"; else FREEZE_ARCH="x86_64"; fi && \
    wget -qO- "https://github.com/charmbracelet/freeze/releases/download/v0.2.2/freeze_0.2.2_Linux_${FREEZE_ARCH}.tar.gz" | tar xz -C /tmp && \
    mv /tmp/freeze_*/freeze /usr/local/bin/freeze && \
    rm -rf /tmp/freeze_* && \
    freeze --version

COPY --from=builder /roborev /usr/local/bin/roborev

# Create config pointing to local daemon
RUN mkdir -p /root/.roborev && \
    echo 'server_addr = "127.0.0.1:7373"' > /root/.roborev/config.toml

# Mock xclip for clipboard operations (atotto/clipboard uses this on Linux)
RUN printf '#!/bin/sh\ncat > /dev/null\n' > /usr/local/bin/xclip && \
    chmod +x /usr/local/bin/xclip

WORKDIR /screenshots
