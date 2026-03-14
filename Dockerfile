# ============================================================
# cmdguard container example
#
# This Dockerfile demonstrates the "Level 3" isolation:
# - Original binaries have execute removed for 'other'
# - cmdguard binary has setgid for the 'cmdexec' group
# - Claude Code user cannot execute binaries directly
# - PATH only contains the guard's bin/ directory
# ============================================================

FROM golang:1.22-bookworm AS builder

WORKDIR /build
COPY go.mod ./
COPY vendor_yaml/ ./vendor_yaml/
COPY main.go ./

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o cmdguard .

# --- Runtime ---
FROM debian:bookworm-slim

# Create the guard group and claude user
RUN groupadd cmdexec && \
    useradd -m -s /bin/bash claude && \
    # DO NOT add claude to cmdexec group - the setgid bit handles this

    # Install the guard
    mkdir -p /opt/cmdguard/bin

COPY --from=builder /build/cmdguard /opt/cmdguard/cmdguard
COPY cmdguard.yaml /opt/cmdguard/cmdguard.yaml

# Set permissions on the guard binary
RUN chown root:cmdexec /opt/cmdguard/cmdguard && \
    chmod 2755 /opt/cmdguard/cmdguard && \
    chown root:root /opt/cmdguard/cmdguard.yaml && \
    chmod 644 /opt/cmdguard/cmdguard.yaml

# Create symlinks for all commands defined in policy
RUN for cmd in git docker curl ls cat; do \
        if command -v "$cmd" >/dev/null 2>&1; then \
            ln -sf /opt/cmdguard/cmdguard /opt/cmdguard/bin/$cmd; \
        fi; \
    done

# Lock down original binaries: remove execute for 'other', add for 'cmdexec'
RUN for cmd in git docker curl ls cat; do \
        real=$(command -v "$cmd" 2>/dev/null || true); \
        if [ -n "$real" ]; then \
            real=$(readlink -f "$real"); \
            chgrp cmdexec "$real"; \
            chmod o-x "$real"; \
            chmod g+rx "$real"; \
        fi; \
    done

# Environment for Claude Code
ENV ORIGINAL_PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ENV PATH=/opt/cmdguard/bin

USER claude
WORKDIR /home/claude

# Verify: claude user cannot directly run locked binaries
# but CAN run them through the guard
RUN echo "=== Verification ===" && \
    echo "Direct /usr/bin/git:" && \
    /usr/bin/git --version 2>&1 || echo "  -> BLOCKED (expected)" && \
    echo "Via guard:" && \
    git status 2>&1 || echo "  -> ran via guard (git not in a repo, but executed)"
