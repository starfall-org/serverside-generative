#!/bin/sh
# Install the latest published ServerSide GitHub release, including pre-releases.
# No root privileges, Go toolchain, jq, or repository checkout required.
set -eu
umask 077

repo=starfall-org/serverside-generative
api=https://api.github.com/repos/$repo/releases
base=https://github.com/$repo/releases/download

fail() { printf 'serverside installer: %s\n' "$*" >&2; exit 1; }
for tool in curl python3 mktemp uname chmod cp mv mkdir rm awk grep; do
    command -v "$tool" >/dev/null 2>&1 || fail "missing required command: $tool"
done

case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) fail 'unsupported OS; prebuilt installers support Linux and macOS only' ;;
esac
case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) fail 'unsupported CPU architecture; requires x86-64 or arm64' ;;
esac
asset="serverside-$os-$arch"

tmp=$(mktemp -d "${TMPDIR:-/tmp}/serverside-install.XXXXXXXX") || fail 'could not create a secure temporary directory'
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM

# releases/latest excludes pre-releases. Read published releases instead.
tag=${SERVERSIDE_TAG:-}
if [ -z "$tag" ]; then
    curl --fail --silent --show-error --location --retry 3 \
        --proto '=https' --proto-redir '=https' "$api?per_page=30" -o "$tmp/releases.json" \
        || fail 'could not list GitHub releases'
    tag=$(python3 - "$tmp/releases.json" <<'PY'
import json
import sys
try:
    with open(sys.argv[1], encoding="utf-8") as stream:
        releases = json.load(stream)
    if not isinstance(releases, list):
        raise ValueError("unexpected GitHub API response")
    published = [r for r in releases if not r.get("draft") and r.get("published_at")]
    if not published:
        raise ValueError("no published release found")
    print(max(published, key=lambda r: r["published_at"])["tag_name"])
except (OSError, ValueError, KeyError, TypeError) as error:
    sys.exit(f"Could not determine latest published release: {error}")
PY
    ) || fail 'could not select a published release'
fi
# The tag becomes part of a URL path; never allow path separators or arbitrary paths.
printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9._-]+)?$' \
    || fail "invalid release tag: $tag"

printf 'Downloading ServerSide %s for %s/%s...\n' "$tag" "$os" "$arch"
for name in "$asset" SHA256SUMS; do
    curl --fail --silent --show-error --location --retry 3 \
        --proto '=https' --proto-redir '=https' "$base/$tag/$name" -o "$tmp/$name" \
        || fail "cannot download $name from release $tag"
done

# Fail closed if the release does not publish exactly one valid matching hash.
expected=$(python3 - "$tmp/SHA256SUMS" "$asset" <<'PY'
import re
import sys
matches = []
with open(sys.argv[1], encoding="ascii") as stream:
    for line in stream:
        found = re.fullmatch(r"([0-9a-fA-F]{64})[ \t]+\*?" + re.escape(sys.argv[2]), line.strip())
        if found:
            matches.append(found.group(1).lower())
if len(matches) != 1:
    sys.exit("Missing or duplicate SHA-256 checksum for selected binary")
print(matches[0])
PY
) || fail 'invalid release checksum manifest'
if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
else
    fail 'sha256sum or shasum is required to verify the downloaded binary'
fi
[ "$actual" = "$expected" ] || fail 'SHA-256 checksum mismatch; binary was not installed'
printf 'SHA-256 verified.\n'

# Prefer an already-present, user-writable PATH directory. Never invoke sudo.
in_path() {
    case ":${PATH:-}:" in
        *":$1:"*) return 0 ;;
        *) return 1 ;;
    esac
}
if [ -n "${INSTALL_DIR:-}" ]; then
    target_dir=$INSTALL_DIR
    case "$target_dir" in /*) ;; *) fail 'INSTALL_DIR must be an absolute path' ;; esac
else
    target_dir=
    for candidate in "$HOME/.local/bin" "$HOME/bin" /usr/local/bin /opt/homebrew/bin; do
        if in_path "$candidate" && [ -d "$candidate" ] && [ -w "$candidate" ]; then
            target_dir=$candidate
            break
        fi
    done
    [ -n "$target_dir" ] || target_dir="$HOME/.local/bin"
fi
mkdir -p "$target_dir" || fail "cannot create $target_dir (use INSTALL_DIR for another directory)"
[ -d "$target_dir" ] && [ -w "$target_dir" ] || fail "directory is not writable: $target_dir"

# Copy to a separate file and rename within the target filesystem atomically.
staged=$(mktemp "$target_dir/.serverside.XXXXXXXX") || fail "cannot stage binary in $target_dir"
trap 'rm -f "$staged"; rm -rf "$tmp"' EXIT
cp "$tmp/$asset" "$staged" || fail 'cannot copy downloaded binary'
chmod 755 "$staged" || fail 'cannot set executable permissions'
mv -f "$staged" "$target_dir/serverside" || fail 'cannot install serverside executable'
printf 'Installed %s to %s/serverside\n' "$tag" "$target_dir"
if in_path "$target_dir"; then
    printf 'Run: serverside version\n'
elif [ -z "${INSTALL_DIR:-}" ] && [ "$target_dir" = "$HOME/.local/bin" ]; then
    # Ensure the fallback directory is discoverable in future login shells.
    # This does not modify the PATH of the invoking shell (which is impossible
    # for an installer run as a subprocess).
    case "${SHELL:-}" in
        */zsh) profile="$HOME/.zprofile" ;;
        */bash)
            if [ -f "$HOME/.bash_profile" ]; then profile="$HOME/.bash_profile"
            elif [ -f "$HOME/.bash_login" ]; then profile="$HOME/.bash_login"
            else profile="$HOME/.profile"; fi ;;
        *) profile="$HOME/.profile" ;;
    esac
    if [ ! -e "$profile" ] || [ -f "$profile" ]; then
        if ! grep -Fq '# ServerSide installer PATH' "$profile" 2>/dev/null; then
            { printf '\n# ServerSide installer PATH\n';
              printf '%s\n' 'case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) PATH="$HOME/.local/bin:$PATH"; export PATH ;; esac';
            } >> "$profile" || fail "cannot update PATH in $profile"
        fi
        printf 'Added ~/.local/bin to PATH in %s. Open a new login shell, then run: serverside version\n' "$profile"
    else
        printf 'Could not edit your shell startup file. Add ~/.local/bin to PATH manually.\n'
        printf 'Run immediately: %s/serverside version\n' "$target_dir"
    fi
else
    printf '\n%s is not in your PATH; add it to your shell profile before invoking serverside by name.\n' "$target_dir"
    printf 'Run immediately: %s/serverside version\n' "$target_dir"
fi
