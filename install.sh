#!/bin/sh
# aish installer — builds/installs the aish binary, a llama.cpp server and the
# default LFM model into a per-user data directory.
#
#   curl -fsSL https://raw.githubusercontent.com/UnderStudioHK/aish/main/install.sh | sh
#   ./install.sh [options]
#
# Re-running is safe: existing components are detected and skipped.
set -eu

REPO="UnderStudioHK/aish"
REPO_URL="https://github.com/$REPO"
LLAMA_REPO="ggml-org/llama.cpp"
LLAMA_TAG="${AISH_LLAMA_TAG:-b12034}"
MODEL_REPO="LiquidAI/LFM3-1.7B-GGUF"
MODEL_FILE="LFM3-1.7B-QAD-Q4_0.gguf"
MODEL_SHA="43a7d09a0e9cba91b63eb7052488b9564569bddfd65754d2e426465e572a7452"
MODEL_BYTES=1049615680

PREFIX="${PREFIX:-$HOME/.local/bin}"
if [ -n "${AISH_HOME:-}" ]; then
  DATA="$AISH_HOME"
else
  DATA="${XDG_DATA_HOME:-$HOME/.local/share}/aish"
fi
BACKEND=auto
SKIP_MODEL=0
FORCE=0
FROM_SOURCE=0
SRC=""
TMP=""

say()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarn:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

cleanup() { [ -z "$TMP" ] || rm -rf "$TMP"; }
trap cleanup EXIT INT TERM

usage() {
  cat <<EOF
aish installer

usage: install.sh [options]

options:
  --prefix DIR      install the aish binary here (default: \$HOME/.local/bin)
  --data-dir DIR    models + llama.cpp location (default: \${XDG_DATA_HOME:-~/.local/share}/aish)
  --cpu             force the CPU llama.cpp build (default: auto-detect Vulkan)
  --vulkan          force the Vulkan llama.cpp build
  --skip-model      do not download the model (bring your own via AISH_MODEL)
  --llama-tag TAG   llama.cpp release tag to fetch (default: $LLAMA_TAG)
  --from-source     build aish from source instead of downloading a release binary
  --force           reinstall llama.cpp/model even if present
  -h, --help        show this help

environment:
  AISH_HOME         overrides the data directory
  PREFIX            same as --prefix
  AISH_LLAMA_TAG    same as --llama-tag
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --prefix) PREFIX="$2"; shift 2 ;;
    --data-dir) DATA="$2"; shift 2 ;;
    --cpu) BACKEND=cpu; shift ;;
    --vulkan) BACKEND=vulkan; shift ;;
    --skip-model) SKIP_MODEL=1; shift ;;
    --llama-tag) LLAMA_TAG="$2"; shift 2 ;;
    --from-source) FROM_SOURCE=1; shift ;;
    --force) FORCE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

say "aish installer — binary: $PREFIX/aish, data: $DATA"

# --- requirements ---------------------------------------------------------
for cmd in curl tar; do
  command -v "$cmd" >/dev/null 2>&1 || die "'$cmd' is required"
done

mkdir -p "$DATA" "$PREFIX" 2>/dev/null || die "cannot create $DATA or $PREFIX"

AVAIL_KB=$(df -Pk "$DATA" 2>/dev/null | awk 'NR==2 {print $4}')
if [ "${AVAIL_KB:-0}" -lt 2200000 ] && [ "$SKIP_MODEL" -eq 0 ]; then
  warn "less than 2.2 GB free under $DATA"
fi

need_go() {
  command -v go >/dev/null 2>&1 || die "Go 1.26+ is required to build from source (https://go.dev/dl/)"
  GOVER=$(go env GOVERSION 2>/dev/null | sed 's/^go//')
  MAJ=$(echo "$GOVER" | cut -d. -f1)
  MIN=$(echo "$GOVER" | cut -d. -f2)
  if [ "${MAJ:-0}" -lt 1 ] || { [ "${MAJ:-0}" -eq 1 ] && [ "${MIN:-0}" -lt 26 ]; }; then
    die "Go 1.26+ required (found $GOVER)"
  fi
}

fetch_source() {
  [ -n "$TMP" ] || TMP=$(mktemp -d "${TMPDIR:-/tmp}/aish-install.XXXXXX")
  say "fetching source from $REPO_URL"
  curl -fsSL --retry 3 "$REPO_URL/archive/refs/heads/main.tar.gz" -o "$TMP/src.tar.gz" \
    || die "could not download the source tarball"
  tar -xzf "$TMP/src.tar.gz" -C "$TMP" || die "could not unpack the source tarball"
  SRC=$(find "$TMP" -maxdepth 2 -name go.mod -exec dirname {} \; | head -1)
  [ -n "$SRC" ] || die "source tarball did not contain go.mod"
}

# --- aish binary ----------------------------------------------------------
BIN_TMP="$PREFIX/.aish-install.$$"
case "$(uname -s)" in
  Linux) ;;
  *) warn "aish is developed and tested on Linux; continuing anyway" ;;
esac
MACHINE=$(uname -m)
case "$MACHINE" in
  x86_64|amd64) AISH_ARCH=amd64; LLAMA_ARCH=x64 ;;
  aarch64|arm64) AISH_ARCH=arm64; LLAMA_ARCH=arm64 ;;
  *) die "unsupported architecture: $MACHINE" ;;
esac

installed_ok=0
if [ -x "$PREFIX/aish" ] && [ "$FORCE" -eq 0 ] && [ "$FROM_SOURCE" -eq 0 ]; then
  say "aish already installed at $PREFIX/aish (use --force to reinstall)"
  installed_ok=1
fi

if [ "$installed_ok" -eq 0 ]; then
  if [ "$FROM_SOURCE" -eq 0 ] && [ -f ./cmd/aish/main.go ] && [ -f ./go.mod ]; then
    SRC=$(pwd)
    FROM_SOURCE=1
    say "building from local source at $SRC"
  elif [ "$FROM_SOURCE" -eq 0 ]; then
    say "downloading the latest aish release binary ($AISH_ARCH)"
    if curl -fL --retry 3 --connect-timeout 20 \
        "$REPO_URL/releases/latest/download/aish-linux-$AISH_ARCH" -o "$BIN_TMP" 2>/dev/null; then
      chmod +x "$BIN_TMP"
      if "$BIN_TMP" --version >/dev/null 2>&1; then
        say "downloaded release binary"
      else
        warn "downloaded binary did not run; falling back to a source build"
        rm -f "$BIN_TMP"
        FROM_SOURCE=1
      fi
    else
      say "no release binary available; building from source"
      FROM_SOURCE=1
    fi
  fi

  if [ "$FROM_SOURCE" -eq 1 ]; then
    need_go
    [ -n "$SRC" ] || fetch_source
    say "building aish"
    ( cd "$SRC" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$BIN_TMP" ./cmd/aish ) \
      || die "go build failed"
  fi

  [ -s "$BIN_TMP" ] || die "aish binary was not produced"
  mv -f "$BIN_TMP" "$PREFIX/aish"
  chmod +x "$PREFIX/aish"
  say "installed $PREFIX/aish"
fi

# --- llama.cpp ------------------------------------------------------------
LLAMA_DIR="$DATA/llama.cpp"
if [ -x "$LLAMA_DIR/llama-server" ] && [ "$FORCE" -eq 0 ]; then
  say "llama.cpp already installed at $LLAMA_DIR"
else
  if [ "$BACKEND" = auto ]; then
    if command -v vulkaninfo >/dev/null 2>&1 && vulkaninfo --summary >/dev/null 2>&1; then
      BACKEND=vulkan
    elif [ -d /dev/dri ] && command -v lspci >/dev/null 2>&1 \
         && lspci 2>/dev/null | grep -qiE 'vga|3d|display'; then
      # A GPU exists but vulkaninfo is unavailable; the Vulkan build still
      # works when the loader is installed at runtime.
      BACKEND=vulkan
    else
      BACKEND=cpu
    fi
  fi
  if [ "$BACKEND" = vulkan ]; then
    ASSET="llama-${LLAMA_TAG}-bin-ubuntu-vulkan-${LLAMA_ARCH}.tar.gz"
  else
    ASSET="llama-${LLAMA_TAG}-bin-ubuntu-${LLAMA_ARCH}.tar.gz"
  fi
  [ -n "$TMP" ] || TMP=$(mktemp -d "${TMPDIR:-/tmp}/aish-install.XXXXXX")
  say "downloading llama.cpp $LLAMA_TAG ($BACKEND, $LLAMA_ARCH)"
  curl -fL --retry 3 --connect-timeout 20 \
    "https://github.com/${LLAMA_REPO}/releases/download/${LLAMA_TAG}/${ASSET}" -o "$TMP/llama.tar.gz" \
    || die "could not download $ASSET"
  tar -xzf "$TMP/llama.tar.gz" -C "$TMP" || die "could not unpack llama.cpp"
  mkdir -p "$LLAMA_DIR"
  cp -a "$TMP"/llama-*/. "$LLAMA_DIR"/ || die "could not install llama.cpp"
  chmod +x "$LLAMA_DIR"/llama-server "$LLAMA_DIR"/llama-cli 2>/dev/null || true
  "$LLAMA_DIR/llama-server" --version >/dev/null 2>&1 \
    || warn "llama-server did not report a version; a Vulkan/CPU runtime may be missing"
  say "llama.cpp installed at $LLAMA_DIR"
fi

# --- model ----------------------------------------------------------------
mkdir -p "$DATA/models"
MODEL_PATH="$DATA/models/$MODEL_FILE"
if [ "$SKIP_MODEL" -eq 1 ]; then
  say "skipping model download (set AISH_MODEL to your GGUF)"
elif [ -s "$MODEL_PATH" ] && [ "$(wc -c <"$MODEL_PATH" | tr -d ' ')" -eq "$MODEL_BYTES" ] && [ "$FORCE" -eq 0 ]; then
  say "model already present at $MODEL_PATH"
else
  [ -n "$TMP" ] || TMP=$(mktemp -d "${TMPDIR:-/tmp}/aish-install.XXXXXX")
  say "downloading $MODEL_FILE (~1.0 GB) from Hugging Face"
  URL="https://huggingface.co/${MODEL_REPO}/resolve/main/${MODEL_FILE}"
  n=0
  until curl -fL --retry 3 --retry-delay 5 --connect-timeout 20 -C - -o "$TMP/$MODEL_FILE" "$URL"; do
    n=$((n+1))
    [ "$n" -ge 20 ] && die "model download failed after $n attempts"
    warn "download interrupted; resuming (attempt $((n+1)))"
    sleep 5
  done
  HAVE=$(wc -c <"$TMP/$MODEL_FILE" | tr -d ' ')
  [ "$HAVE" -eq "$MODEL_BYTES" ] || die "model size mismatch: $HAVE bytes, expected $MODEL_BYTES"
  if command -v sha256sum >/dev/null 2>&1; then
    SUM=$(sha256sum "$TMP/$MODEL_FILE" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    SUM=$(shasum -a 256 "$TMP/$MODEL_FILE" | awk '{print $1}')
  else
    die "sha256sum or shasum is required to verify the model"
  fi
  [ "$SUM" = "$MODEL_SHA" ] || die "model checksum mismatch (got $SUM)"
  mv -f "$TMP/$MODEL_FILE" "$MODEL_PATH"
  say "model verified and installed at $MODEL_PATH"
fi

# --- report ---------------------------------------------------------------
AISH_LLM_BIN="$LLAMA_DIR/llama-server" AISH_MODEL="$MODEL_PATH" \
  "$PREFIX/aish" doctor || warn "doctor reported issues (see above)"

case ":$PATH:" in
  *":$PREFIX:"*) ;;
  *) warn "$PREFIX is not on your PATH. Add it, e.g.:
      echo 'export PATH=\"$PREFIX:\$PATH\"' >> ~/.zshrc" ;;
esac

cat <<EOF

Done. Start a shell with:

  aish

The first natural-language request auto-starts llama-server (a few seconds);
after that, local turns are ~1–3 s on a modern laptop.
Docs: $REPO_URL
EOF
