#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# Sanitised environment dump for bug reports. Paste the output into a GitHub
# issue. Secrets are never printed — only whether they are set and how long
# they are.
#
# POSIX sh, no dependencies: this has to work on the machine where mimux itself
# will not start, which is the whole reason it exists.

set -u

redact() {
	# $1 = var name. Prints "set (N chars)" or "unset" — never the value.
	eval "v=\${$1:-}"
	if [ -n "$v" ]; then
		printf '%s = set (%s chars)\n' "$1" "$(printf '%s' "$v" | wc -c | tr -d ' ')"
	else
		printf '%s = unset\n' "$1"
	fi
}

show() {
	eval "v=\${$1:-}"
	printf '%s = %s\n' "$1" "${v:-<unset, using default>}"
}

have() { command -v "$1" >/dev/null 2>&1; }

echo '## mimux diagnose'
echo
date -u '+generated: %Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date

echo
echo '### version'
if [ -x ./bin/mimux ]; then
	./bin/mimux -version 2>&1
elif have mimux; then
	mimux -version 2>&1
else
	echo 'binary not found (./bin/mimux or $PATH) — running from Docker?'
fi
git -C "$(dirname "$0")/.." rev-parse --short HEAD 2>/dev/null | sed 's/^/git commit: /'

echo
echo '### host'
uname -srm
echo "libc: $(ldd --version 2>&1 | head -1 || echo unknown)"
[ -r /etc/os-release ] && . /etc/os-release && echo "os: ${PRETTY_NAME:-unknown}"
echo "cpus: $(nproc 2>/dev/null || echo unknown)"
echo "in container: $([ -f /.dockerenv ] && echo yes || echo no)"
echo "tz: $(date '+%Z %z')"

echo
echo '### toolchain'
have go && go version || echo 'go: not installed'
for t in node npm docker sqlite3; do
	if have "$t"; then
		printf '%s: ' "$t"
		"$t" --version 2>&1 | head -1
	else
		echo "$t: not installed"
	fi
done
if have docker; then
	printf 'docker compose: '
	docker compose version 2>&1 | head -1 || echo 'not available'
fi

echo
echo '### config (env)'
# Values that cannot identify or authenticate anything.
for v in MIMUX_HOST MIMUX_PORT MIMUX_DB MIMUX_AI_BASE_URL; do show "$v"; done
# BASE_URL can contain a private hostname; show only the scheme, which is the
# part that actually causes bugs (Secure cookies, OAuth redirect mismatch).
echo "MIMUX_BASE_URL scheme = $(printf '%s' "${MIMUX_BASE_URL:-}" | sed -n 's|^\([a-z]*\)://.*|\1|p' | grep . || echo '<unset, using default>')"
for v in MIMUX_SECRET; do redact "$v"; done
# Anything else in the environment that looks like ours, names only.
echo 'other MIMUX_/SM_ vars set:'
env | sed -n 's/^\(MIMUX_[A-Z_]*\|SM_[A-Z_]*\)=.*/  \1/p' |
	grep -v -E '  (MIMUX_HOST|MIMUX_PORT|MIMUX_DB|MIMUX_BASE_URL|MIMUX_SECRET|MIMUX_AI_BASE_URL)$' || echo '  (none)'

echo
echo '### database'
db="${MIMUX_DB:-./data/mimux.db}"
echo "path: $db"
if [ -f "$db" ]; then
	ls -l "$db" | awk '{print "size: "$5" bytes, mode: "$1", owner: "$3":"$4}'
	for s in -wal -shm; do
		[ -f "$db$s" ] && ls -l "$db$s" | awk -v s="$s" '{print "sidecar "s": "$5" bytes"}'
	done
	if have sqlite3; then
		echo "journal_mode: $(sqlite3 "$db" 'PRAGMA journal_mode;' 2>&1)"
		echo "integrity: $(sqlite3 "$db" 'PRAGMA quick_check;' 2>&1 | head -1)"
		echo "schema_version: $(sqlite3 "$db" 'PRAGMA user_version;' 2>&1)"
		echo "accounts: $(sqlite3 "$db" 'SELECT count(*) FROM accounts;' 2>&1)"
		echo "messages: $(sqlite3 "$db" 'SELECT count(*) FROM messages;' 2>&1)"
	else
		echo 'sqlite3 not installed — install it for schema/row counts'
	fi
else
	echo 'DOES NOT EXIST (fresh install, or the path is wrong)'
	d=$(dirname "$db")
	[ -d "$d" ] && ls -ld "$d" | awk '{print "parent dir: mode "$1", owner "$3":"$4}' ||
		echo "parent dir $d does not exist either"
fi
[ -f "$(dirname "$db")/secret" ] && echo 'secret file: present' || echo 'secret file: absent'

echo
echo '### listener'
port="${MIMUX_PORT:-8083}"
if have ss; then
	ss -ltnp 2>/dev/null | grep ":$port " || echo "nothing listening on :$port"
elif have netstat; then
	netstat -ltn 2>/dev/null | grep ":$port " || echo "nothing listening on :$port"
else
	echo 'ss/netstat unavailable'
fi
if have curl; then
	echo "GET /login -> $(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:$port/login" 2>&1 || echo unreachable)"
fi

echo
echo '### recent container logs (last 30 lines)'
if have docker && [ -f docker-compose.yml ]; then
	docker compose logs --tail=30 --no-color 2>&1 | tail -30 || echo 'no compose logs'
else
	echo 'not a compose checkout — paste your own logs below'
fi

echo
echo '--- end of diagnose. Check for anything you consider private before posting. ---'
