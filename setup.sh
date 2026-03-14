#!/bin/bash
set -euo pipefail

# ============================================================
# claude-guard setup script
#
# This script:
# 1. Compiles the claude-guard binary
# 2. Creates the 'claude-guard' group
# 3. Sets up the binary with setgid
# 4. Creates symlinks in a bin/ directory
# 5. Optionally locks down original binaries
#
# Usage:
#   sudo ./setup.sh [--install-dir /opt/claude-guard] [--lock-binaries]
#
# After setup, configure Claude Code's environment:
#   PATH=/opt/claude-guard/bin
#   ORIGINAL_PATH=<original system PATH>
# ============================================================

INSTALL_DIR="/opt/claude-guard"
LOCK_BINARIES=false
GUARD_GROUP="claude-guard"
CLAUDE_USER="${CLAUDE_USER:-claude}"

usage() {
    cat <<EOF
Usage: $0 [OPTIONS]

Options:
  --install-dir DIR    Installation directory (default: /opt/claude-guard)
  --lock-binaries      Remove 'other' execute permission from original binaries
                       and add execute for the claude-guard group only
  --claude-user USER   User that Claude Code runs as (default: claude)
  --group NAME         Group name for setgid (default: claude-guard)
  -h, --help           Show this help

After installation:
  Set these environment variables for Claude Code:
    ORIGINAL_PATH=\$PATH
    PATH=$INSTALL_DIR/bin

EOF
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --install-dir)   INSTALL_DIR="$2"; shift 2 ;;
        --lock-binaries) LOCK_BINARIES=true; shift ;;
        --claude-user)   CLAUDE_USER="$2"; shift 2 ;;
        --group)         GUARD_GROUP="$2"; shift 2 ;;
        -h|--help)       usage ;;
        *)               echo "Unknown option: $1"; usage ;;
    esac
done

BIN_DIR="$INSTALL_DIR/bin"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "=== claude-guard setup ==="
echo "  Install dir:    $INSTALL_DIR"
echo "  Bin dir:        $BIN_DIR"
echo "  Group:          $GUARD_GROUP"
echo "  Claude user:    $CLAUDE_USER"
echo "  Lock binaries:  $LOCK_BINARIES"
echo ""

# --- Check root ---
if [[ $EUID -ne 0 ]]; then
    echo "ERROR: This script must be run as root (sudo)."
    exit 1
fi

# --- Check Go ---
if ! command -v go &>/dev/null; then
    echo "ERROR: Go compiler not found. Install Go first."
    exit 1
fi

# --- Create group ---
if ! getent group "$GUARD_GROUP" &>/dev/null; then
    echo "[+] Creating group: $GUARD_GROUP"
    groupadd "$GUARD_GROUP"
else
    echo "[=] Group $GUARD_GROUP already exists"
fi

# --- Build binary ---
echo "[+] Building claude-guard binary..."
cd "$SCRIPT_DIR"
CGO_ENABLED=0 go build -ldflags="-s -w" -o claude-guard .
echo "    Built: claude-guard ($(stat -c%s claude-guard) bytes)"

# --- Install ---
echo "[+] Installing to $INSTALL_DIR"
mkdir -p "$INSTALL_DIR" "$BIN_DIR"

cp claude-guard "$INSTALL_DIR/claude-guard"
cp claude-guard.yaml "$INSTALL_DIR/claude-guard.yaml"

# Binary: owned by root:claude-guard, setgid, no write for group/other
chown root:"$GUARD_GROUP" "$INSTALL_DIR/claude-guard"
chmod 2755 "$INSTALL_DIR/claude-guard"

# Policy: readable by root only (Claude cannot modify policy)
chown root:root "$INSTALL_DIR/claude-guard.yaml"
chmod 644 "$INSTALL_DIR/claude-guard.yaml"

# --- Create symlinks from policy ---
echo "[+] Creating symlinks from policy..."
# Extract command names from YAML (simple grep, works for our format)
COMMANDS=$(grep -E '^\s{2}\w' "$INSTALL_DIR/claude-guard.yaml" | \
           grep -v '^\s*#' | \
           grep -v 'commands:' | \
           grep -v 'global_options' | \
           grep -v 'subcommands' | \
           grep -v 'allow_bare' | \
           grep -v 'bare_options' | \
           sed 's/://g' | \
           awk '{print $1}' | \
           sort -u)

for cmd in $COMMANDS; do
    # Verify the command actually exists on the system
    if command -v "$cmd" &>/dev/null; then
        ln -sf "$INSTALL_DIR/claude-guard" "$BIN_DIR/$cmd"
        echo "    $BIN_DIR/$cmd -> claude-guard"
    else
        echo "    SKIP: $cmd (not found on system)"
    fi
done

# --- Lock down original binaries (optional) ---
if [[ "$LOCK_BINARIES" == "true" ]]; then
    echo ""
    echo "[+] Locking down original binaries..."
    echo "    (removing 'other' execute, adding group execute for $GUARD_GROUP)"

    for cmd in $COMMANDS; do
        REAL_PATH=$(command -v "$cmd" 2>/dev/null || true)
        if [[ -z "$REAL_PATH" ]]; then
            continue
        fi
        # Resolve symlinks
        REAL_PATH=$(readlink -f "$REAL_PATH")

        echo "    Locking: $REAL_PATH"
        # Change group to claude-guard, keep owner
        chgrp "$GUARD_GROUP" "$REAL_PATH"
        # Set permissions: owner rwx, group rx (claude-guard can execute), other r-- (no execute)
        chmod o-x "$REAL_PATH"
        chmod g+rx "$REAL_PATH"
    done

    # Add claude user to the guard group so the setgid binary works
    if id "$CLAUDE_USER" &>/dev/null; then
        echo ""
        echo "[+] Note: $CLAUDE_USER should NOT be in group $GUARD_GROUP"
        echo "    The setgid bit on claude-guard grants group access at execution time."
        echo "    If $CLAUDE_USER were in the group, they could run binaries directly."

        if id -nG "$CLAUDE_USER" | grep -qw "$GUARD_GROUP"; then
            echo "    WARNING: $CLAUDE_USER IS in group $GUARD_GROUP - removing..."
            gpasswd -d "$CLAUDE_USER" "$GUARD_GROUP" 2>/dev/null || true
        fi
    fi
fi

echo ""
echo "=== Setup complete ==="
echo ""
echo "To use with Claude Code, set the environment:"
echo ""
echo "  export ORIGINAL_PATH=\"\$PATH\""
echo "  export PATH=\"$BIN_DIR\""
echo ""
echo "Or in a container Dockerfile:"
echo ""
echo "  ENV ORIGINAL_PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
echo "  ENV PATH=$BIN_DIR"
echo ""
echo "To edit allowed commands/options:"
echo "  sudo vi $INSTALL_DIR/claude-guard.yaml"
echo ""

if [[ "$LOCK_BINARIES" == "true" ]]; then
    echo "Binary lockdown is ACTIVE."
    echo "  - Original binaries: execute removed for 'other' users"
    echo "  - claude-guard (setgid): can execute as group $GUARD_GROUP"
    echo "  - Claude user ($CLAUDE_USER) cannot directly execute locked binaries"
    echo ""
    echo "To verify: run 'ls -la \$(which git)' - should show no 'x' in other perms"
fi
