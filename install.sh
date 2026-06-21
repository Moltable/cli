#!/bin/sh
# moltable CLI install script.
#
# Usage:
#   curl -fsSL https://get.moltable.io | sh
#
# Environment:
#   MOLTABLE_INSTALL_DIR  Directory to install the binary into.
#                         Defaults to /usr/local/bin (falls back to
#                         ~/.local/bin if /usr/local/bin is not writable).
#   MOLTABLE_VERSION      Pin a specific version (e.g. "0.1.0"). Defaults
#                         to the latest GitHub release tagged "v*".
#
# Behavior:
#   1. Detect OS (linux/darwin) and arch (amd64/arm64) via uname.
#   2. Fetch the latest "v*" release tag from the GitHub API.
#   3. Download the matching tarball + checksums.txt.
#   4. Verify the tarball's sha256 against checksums.txt.
#   5. Extract the binary, install to MOLTABLE_INSTALL_DIR.
#   6. Run `moltable skills install` so Claude Code can find the bundle.
#   7. Print a "run moltable auth login next" hint.

set -eu

REPO="moltable/cli"
DEFAULT_INSTALL_DIR="/usr/local/bin"
FALLBACK_INSTALL_DIR="$HOME/.local/bin"
GH_API="https://api.github.com/repos/${REPO}/releases"
GH_DL="https://github.com/${REPO}/releases/download"

# ---- output helpers --------------------------------------------------

info() {
    printf '%s\n' "$*"
}

warn() {
    printf 'warning: %s\n' "$*" >&2
}

fail() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

# ---- prerequisites ---------------------------------------------------

require_tool() {
    if ! command -v "$1" >/dev/null 2>&1; then
        fail "Required tool '$1' is not installed. Install it and re-run."
    fi
}

# ---- OS / arch detection ---------------------------------------------

detect_os() {
    uname_s=$(uname -s)
    case "$uname_s" in
        Linux)  echo "linux" ;;
        Darwin) echo "darwin" ;;
        *)
            fail "Unsupported OS: $uname_s. moltable supports linux and darwin via this installer; Windows users should download a binary from https://github.com/${REPO}/releases."
            ;;
    esac
}

detect_arch() {
    uname_m=$(uname -m)
    case "$uname_m" in
        x86_64|amd64)  echo "amd64" ;;
        arm64|aarch64) echo "arm64" ;;
        *)
            fail "Unsupported architecture: $uname_m. moltable ships amd64 and arm64 builds; see https://github.com/${REPO}/releases for the full asset list."
            ;;
    esac
}

# ---- release lookup --------------------------------------------------

# fetch_latest_tag pulls the latest release with a tag that starts
# with "v" — the prefix that goreleaser cuts. We parse the JSON
# without jq so the install script stays dependency-free.
fetch_latest_tag() {
    if [ -n "${MOLTABLE_VERSION:-}" ]; then
        echo "v${MOLTABLE_VERSION#v}"
        return 0
    fi
    # The /releases endpoint returns the 30 most recent releases in
    # reverse-chronological order. We grep for the first tag_name that
    # starts with "v" so unrelated tags (api-v*, vN.N.N web tags)
    # are skipped.
    body=$(curl -fsSL "${GH_API}?per_page=30" 2>/dev/null) || \
        fail "Failed to query GitHub releases. Check your internet connection or try again later."
    tag=$(printf '%s\n' "$body" \
        | grep -E '"tag_name"\s*:\s*"v' \
        | head -n 1 \
        | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')
    if [ -z "$tag" ]; then
        fail "Could not find a CLI release on github.com/${REPO}. Has the first v* tag been pushed yet?"
    fi
    echo "$tag"
}

# ---- download + verify -----------------------------------------------

# verify_sha256 picks the right sha256 tool per platform.
#
# We prefer `shasum -a 256` over `sha256sum`: macOS ships a Darwin-
# native `sha256sum` at /sbin/sha256sum ("sha256sum (Darwin) 1.0")
# that does NOT support the `-c` verify flag — only GNU coreutils
# does. A naive "sha256sum first, shasum fallback" picks the wrong
# tool on every Mac and silently fails verification. shasum is a
# Perl script that's standard on macOS AND ships with Perl on every
# mainstream Linux distro, so it's the safer default.
verify_sha256() {
    archive_path="$1"
    checksums_path="$2"
    archive_name=$(basename "$archive_path")
    work_dir=$(dirname "$archive_path")

    if command -v shasum >/dev/null 2>&1; then
        check_cmd="shasum -a 256"
    elif command -v sha256sum >/dev/null 2>&1; then
        check_cmd="sha256sum"
    else
        fail "Neither shasum nor sha256sum is installed. Install one and re-run."
    fi

    # Extract the line for our archive only — checksums.txt contains
    # one entry per asset, and -c will complain about missing files
    # if we pass the whole file blindly.
    line=$(grep -E "  ${archive_name}\$" "$checksums_path" || true)
    if [ -z "$line" ]; then
        fail "Checksum for ${archive_name} not found in checksums.txt. The release may be malformed; please file an issue."
    fi

    # Run the checker from the archive's directory so the relative
    # path in the checksums line resolves.
    ( cd "$work_dir" && printf '%s\n' "$line" | $check_cmd -c >/dev/null 2>&1 ) || \
        fail "Checksum verification failed for ${archive_name}. Aborting install — the download may be corrupted or tampered with."
}

download_and_verify() {
    tag="$1"
    version="$2"
    os="$3"
    arch="$4"
    work_dir="$5"

    archive_name="moltable_${version}_${os}_${arch}.tar.gz"
    archive_url="${GH_DL}/${tag}/${archive_name}"
    checksums_url="${GH_DL}/${tag}/checksums.txt"

    info "Downloading ${archive_name}…"
    curl -fsSL -o "${work_dir}/${archive_name}" "$archive_url" || \
        fail "Failed to download ${archive_name}. The release asset may not exist for your platform (${os}/${arch}). Browse https://github.com/${REPO}/releases/tag/${tag} for the full list."

    curl -fsSL -o "${work_dir}/checksums.txt" "$checksums_url" || \
        fail "Failed to download checksums.txt. Try again or report at https://github.com/${REPO}/issues."

    verify_sha256 "${work_dir}/${archive_name}" "${work_dir}/checksums.txt"
}

# ---- install ---------------------------------------------------------

# choose_install_dir respects MOLTABLE_INSTALL_DIR, then tries
# /usr/local/bin (writable), then falls back to ~/.local/bin. If we
# fall back, we warn about PATH so the binary is actually reachable.
choose_install_dir() {
    if [ -n "${MOLTABLE_INSTALL_DIR:-}" ]; then
        mkdir -p "$MOLTABLE_INSTALL_DIR" || \
            fail "Cannot create MOLTABLE_INSTALL_DIR=${MOLTABLE_INSTALL_DIR}. Check permissions."
        echo "$MOLTABLE_INSTALL_DIR"
        return 0
    fi
    if [ -w "$DEFAULT_INSTALL_DIR" ] || ( [ ! -e "$DEFAULT_INSTALL_DIR" ] && mkdir -p "$DEFAULT_INSTALL_DIR" 2>/dev/null ); then
        echo "$DEFAULT_INSTALL_DIR"
        return 0
    fi
    mkdir -p "$FALLBACK_INSTALL_DIR" || \
        fail "Could not create ${FALLBACK_INSTALL_DIR}. Set MOLTABLE_INSTALL_DIR to a writable directory and re-run."
    echo "$FALLBACK_INSTALL_DIR"
}

extract_binary() {
    work_dir="$1"
    archive_name="$2"
    tar -xzf "${work_dir}/${archive_name}" -C "$work_dir" || \
        fail "Failed to extract ${archive_name}. Is tar installed?"
    if [ ! -f "${work_dir}/moltable" ]; then
        fail "Extracted archive did not contain a 'moltable' binary. Please report this at https://github.com/${REPO}/issues."
    fi
}

install_binary() {
    work_dir="$1"
    install_dir="$2"
    target="${install_dir}/moltable"

    # mv + chmod so we don't leave a half-written binary if mv fails.
    mv "${work_dir}/moltable" "$target" || \
        fail "Failed to move binary into ${install_dir}. Check permissions or set MOLTABLE_INSTALL_DIR."
    chmod 0755 "$target" || \
        fail "Failed to chmod ${target}."
}

run_skills_install() {
    install_dir="$1"
    target="${install_dir}/moltable"
    # Best-effort: if `skills install` errors (e.g. HOME not set in
    # an unusual env), we don't fail the whole install — we just warn
    # and let the user retry. The binary itself is already on PATH.
    if ! "$target" skills install >/dev/null 2>&1; then
        warn "moltable installed, but 'moltable skills install' failed. Run it manually to drop the Claude Code skills bundle into ~/.claude/plugins/moltable/."
    fi
}

# ---- main ------------------------------------------------------------

main() {
    require_tool curl
    require_tool tar
    require_tool grep
    require_tool sed

    os=$(detect_os)
    arch=$(detect_arch)
    tag=$(fetch_latest_tag)
    version=${tag#v}

    work_dir=$(mktemp -d 2>/dev/null || mktemp -d -t moltable)
    # shellcheck disable=SC2064
    trap "rm -rf '$work_dir'" EXIT INT TERM

    download_and_verify "$tag" "$version" "$os" "$arch" "$work_dir"

    archive_name="moltable_${version}_${os}_${arch}.tar.gz"
    extract_binary "$work_dir" "$archive_name"

    install_dir=$(choose_install_dir)
    install_binary "$work_dir" "$install_dir"
    run_skills_install "$install_dir"

    info "moltable ${version} installed to ${install_dir}/moltable"

    # PATH hint when we landed in ~/.local/bin and it's not on PATH.
    case ":${PATH}:" in
        *":${install_dir}:"*) ;;
        *)
            warn "${install_dir} is not on your PATH. Add this to your shell profile:"
            # $PATH here is intentionally literal — the user copies the
            # printed line into their shell rc, where the expansion
            # should happen at *their* shell startup, not ours.
            # shellcheck disable=SC2016
            printf '    export PATH="%s:$PATH"\n' "$install_dir" >&2
            ;;
    esac

    info "Run 'moltable auth login' to get started."
}

main
