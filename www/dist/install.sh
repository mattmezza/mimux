#!/usr/bin/env bash
# mimux installer — https://mimux.dev/install.sh
#
#   curl -fsSL https://mimux.dev/install.sh | bash            # free client
#   curl -fsSL https://mimux.dev/install.sh | bash -s -- pro  # + automation layer
#
# Downloads one release binary from GitHub, verifies its sha256 against the
# checksums.txt published with the release, and drops it in the first writable
# of /usr/local/bin or ~/.local/bin. Nothing else: no daemon, no package
# manager, no sudo behind your back.
#
# Everything is a function and main() is called on the very last line, so a
# download that gets truncated mid-flight defines a few functions and exits
# rather than running half an install.

set -euo pipefail

REPO_URL=${MIMUX_INSTALL_BASE:-https://github.com/mattmezza/mimux/releases}
PRICING_URL=https://mimux.dev/pricing/
PORT=8083

flavour=""
version=""
bin_dir=""
interactive=1
as_mimux=""   # "" ask, 1 yes, 0 no
force=0
# Global, not a local in main(): the EXIT trap runs after main() has returned,
# by which point a local would be out of scope and set -u would fire.
tmp=""

say()  { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<EOF
mimux installer

  install.sh [free|pro] [options]

  free                 the complete mail client, AGPL-3.0 (default)
  pro                  adds the REST API, MCP, webhooks and the mimux CLI;
                       14-day trial built in, licences at $PRICING_URL

  --version vX.Y.Z     install an exact release (default: latest)
  --bin-dir DIR        where to put the binary
  --non-interactive    never prompt; use the arguments and defaults
  --as-mimux           with pro and --non-interactive: install it as 'mimux'
  --force              overwrite an existing binary without asking
  -h, --help           this
EOF
}

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
			free|pro)          flavour=$1 ;;
			--version)         version=${2:-}; [ -n "$version" ] || die "--version needs a tag, e.g. --version v0.21.0"; shift ;;
			--version=*)       version=${1#*=} ;;
			--bin-dir)         bin_dir=${2:-}; [ -n "$bin_dir" ] || die "--bin-dir needs a directory"; shift ;;
			--bin-dir=*)       bin_dir=${1#*=} ;;
			--non-interactive) interactive=0 ;;
			--as-mimux)        as_mimux=1 ;;
			--force)           force=1 ;;
			-h|--help)         usage; exit 0 ;;
			*)                 die "unknown argument: $1 (try --help)" ;;
		esac
		shift
	done
	# A pipe is not a terminal, so `curl | bash` can never prompt. That is the
	# common case, not an error — it just means every question takes its default.
	[ -t 0 ] || interactive=0
}

# uname reports a dozen spellings for two architectures. Anything not on this
# list is a platform we publish no binary for, and saying so beats a 404.
detect_platform() {
	local os arch
	os=$(uname -s)
	arch=$(uname -m)
	case "$os" in
		Linux)  os=linux ;;
		Darwin) os=darwin ;;
		MINGW*|MSYS*|CYGWIN*|Windows_NT)
			die "Windows has no native build. Run the container instead —
  docker run -d -p $PORT:$PORT -v mimux-data:/data ghcr.io/mattmezza/mimux:latest
or grab a binary by hand from $REPO_URL" ;;
		*) die "unsupported operating system: $os — see $REPO_URL" ;;
	esac
	case "$arch" in
		x86_64|amd64)        arch=amd64 ;;
		aarch64|arm64)       arch=arm64 ;;
		*) die "unsupported architecture: $arch (mimux ships amd64 and arm64) — see $REPO_URL" ;;
	esac
	printf '%s-%s\n' "$os" "$arch"
}

choose_flavour() {
	[ -z "$flavour" ] || return 0
	if [ "$interactive" -eq 0 ]; then
		flavour=free
		say "Installing the free client. For the pro one:"
		say "  curl -fsSL https://mimux.dev/install.sh | bash -s -- pro"
		say ""
		return 0
	fi
	cat <<EOF
mimux comes in two builds:

  free   the complete mail client — unified inbox, search, filters, compose.
         AGPL-3.0, nothing held back, no account anywhere.
  pro    all of that plus the automation layer: REST API, MCP server,
         webhooks and the mimux command-line client. 14-day trial is built
         in; licences at $PRICING_URL

EOF
	local answer
	read -r -p "Which one? [free/pro] (free): " answer || answer=""
	case "${answer:-free}" in
		""|free|f|Free|FREE) flavour=free ;;
		pro|p|Pro|PRO)       flavour=pro ;;
		*) die "did not understand '$answer' — say free or pro" ;;
	esac
}

# One of these two exists on every machine that can run mimux: coreutils on
# Linux, shasum (Perl) on macOS. Without one we cannot verify, and installing
# an unverified binary is not on the menu.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "need sha256sum or shasum to verify the download; install coreutils"
	fi
}

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		die "need curl or wget"
	fi
}

download_url() {
	if [ -n "$version" ]; then
		printf '%s/download/%s/%s\n' "$REPO_URL" "$version" "$1"
	else
		printf '%s/latest/download/%s\n' "$REPO_URL" "$1"
	fi
}

# Downloads the binary and checksums.txt into $tmp and verifies one against the
# other. A mismatch is fatal and deliberately unhelpful about carrying on.
download_and_verify() {
	local asset=$1 tmp=$2 want got
	say "Downloading $asset ..."
	fetch "$(download_url "$asset")" "$tmp/$asset" \
		|| die "could not download $asset from $(download_url "$asset")
That release may not carry a build for this platform. Have a look at $REPO_URL"
	fetch "$(download_url checksums.txt)" "$tmp/checksums.txt" \
		|| die "could not download checksums.txt — releases before v0.22 do not
publish one, so there is nothing to verify this download against. Pin a newer
release with --version, or download by hand from $REPO_URL"

	want=$(awk -v f="$asset" '$2 == f || $2 == "*" f {print $1; exit}' "$tmp/checksums.txt")
	[ -n "$want" ] || die "checksums.txt has no entry for $asset — refusing to install"
	got=$(sha256_of "$tmp/$asset")
	[ "$want" = "$got" ] || die "checksum mismatch for $asset
  expected $want
  got      $got
Refusing to install. Try again; if it keeps happening, report it."
	say "Checksum verified."
}

# /usr/local/bin when it is ours to write to, ~/.local/bin otherwise. Never
# sudo — if root is what it takes, that is the user's call to make, and the
# command to make it is printed at the end instead.
choose_bin_dir() {
	local d
	if [ -n "$bin_dir" ]; then
		mkdir -p "$bin_dir" 2>/dev/null || true
		printf '%s\n' "$bin_dir"
		return 0
	fi
	for d in /usr/local/bin "$HOME/.local/bin"; do
		if [ -d "$d" ] && [ -w "$d" ]; then
			printf '%s\n' "$d"
			return 0
		fi
	done
	d=$HOME/.local/bin
	mkdir -p "$d" || die "cannot create $d"
	printf '%s\n' "$d"
}

on_path() {
	case ":${PATH}:" in
		*":$1:"*) return 0 ;;
		*) return 1 ;;
	esac
}

# Pro ships as mimux-pro so it can sit next to a free build, but most people
# want to type `mimux`. Ask, unless we were told.
resolve_name() {
	local answer
	[ "$flavour" = pro ] || { printf 'mimux\n'; return 0; }
	if [ -z "$as_mimux" ]; then
		if [ "$interactive" -eq 1 ]; then
			read -r -p "Install it as 'mimux' rather than 'mimux-pro'? [Y/n]: " answer || answer=""
			case "${answer:-y}" in
				n|N|no|No|NO) as_mimux=0 ;;
				*)            as_mimux=1 ;;
			esac
		else
			as_mimux=0
		fi
	fi
	[ "$as_mimux" -eq 1 ] && printf 'mimux\n' || printf 'mimux-pro\n'
}

# Only asked when pro is about to land on the name a free build already owns —
# a plain free-over-free install is an upgrade and needs no ceremony.
confirm_overwrite() {
	local target=$1 answer
	[ -e "$target" ] || return 0
	[ "$flavour" = pro ] && [ "$(basename "$target")" = mimux ] || return 0
	[ "$force" -eq 0 ] || return 0
	if [ "$interactive" -eq 0 ]; then
		die "$target already exists. Re-run with --force to replace it, or drop
--as-mimux to install alongside it as mimux-pro."
	fi
	read -r -p "$target already exists. Replace it? [y/N]: " answer || answer=""
	case "${answer:-n}" in
		y|Y|yes|Yes|YES) : ;;
		*) die "left $target alone. Re-run without --as-mimux to install as mimux-pro." ;;
	esac
}

install_binary() {
	local src=$1 target=$2
	if [ -d "$(dirname "$target")" ] && [ -w "$(dirname "$target")" ]; then
		install -m 755 "$src" "$target" 2>/dev/null || {
			chmod 755 "$src"
			cp -f "$src" "$target"
		}
	else
		die "$(dirname "$target") is not writable. Either pick another directory
with --bin-dir, or run this yourself:
  sudo install -m 755 $src $target"
	fi
}

next_steps() {
	local name=$1 dir=$2
	say ""
	say "Installed $name to $dir/$name"
	on_path "$dir" || warn "$dir is not on your PATH — add it to your shell profile:
  export PATH=\"$dir:\$PATH\""
	say ""
	say "Start the server:"
	say "  $name"
	say "then open http://localhost:$PORT — the first visit creates your admin account."
	if [ "$flavour" = pro ]; then
		say ""
		say "Or use it as a terminal client against a mimux you already run:"
		say "  $name mail login https://mail.example.com"
	fi
	say ""
	say "Later on, '$name upgrade' replaces it with the newest release."
	# Explicit, because this is the last thing main() runs and so the script's
	# exit status: a bare `if` that happens to be false would report a failure
	# for an install that went perfectly.
	return 0
}

main() {
	parse_args "$@"
	local platform asset name dir target
	platform=$(detect_platform)
	choose_flavour
	if [ "$flavour" = pro ]; then
		asset="mimux-pro-$platform"
	else
		asset="mimux-$platform"
	fi

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	download_and_verify "$asset" "$tmp"

	name=$(resolve_name)
	dir=$(choose_bin_dir)
	target="$dir/$name"
	confirm_overwrite "$target"
	install_binary "$tmp/$asset" "$target"
	next_steps "$name" "$dir"
}

main "$@"
