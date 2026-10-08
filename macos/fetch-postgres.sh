#!/usr/bin/env bash
# Fetch the PostgreSQL the app bundles: a pinned EnterpriseDB macOS build (universal, arm64
# and x86_64), checked against a pinned sha256, cut down to what the app runs.
#
#   macos/fetch-postgres.sh [--out DIR]     default: macos/build/postgres
#
# The archive is 437 MB (it carries pgAdmin and StackBuilder); what is kept is the server,
# the client tools the app and `conductor db` call, the shared libraries, and share/. The
# archive is deleted afterwards. Run on a Mac to also check the result's architectures and
# run `postgres --version`; elsewhere those checks are skipped, saying so.
#
# To move to a newer 17.x: change the three pins below together. The hash is of the zip
# exactly as get.enterprisedb.com serves it.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"

PG_VERSION="17.11-1"
PG_URL="https://get.enterprisedb.com/postgresql/postgresql-17.11-1-osx-binaries.zip"
PG_SHA256="a540ac46eea5a2c612a8e097fd9aa75e41ac3f3b49bf10ffd75ecf5b1efc2997"

# What the app runs: the server and initdb; pg_ctl (fast shutdown), pg_isready and psql
# (readiness, pg_is_in_recovery), createdb; pg_basebackup and pg_controldata for `conductor
# db`; pg_dump, pg_restore and pg_upgrade for moving between major versions later.
KEEP_BIN=(postgres initdb pg_ctl pg_isready psql createdb pg_basebackup pg_controldata
          pg_dump pg_restore pg_upgrade pg_waldump pg_archivecleanup)

die() { echo "fetch-postgres: $*" >&2; exit 1; }

OUT="$HERE/build/postgres"
while [ $# -gt 0 ]; do
    case "$1" in
        --out) [ $# -ge 2 ] || die "--out needs a value"; OUT="$2"; shift 2 ;;
        -h|--help) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) die "unknown argument: $1" ;;
    esac
done

if [ -x "$OUT/bin/postgres" ] && [ "$(cat "$OUT/VERSION" 2>/dev/null | head -1)" = "$PG_VERSION $PG_SHA256" ]; then
    echo "fetch-postgres: $OUT already holds PostgreSQL $PG_VERSION"
    exit 0
fi

# OUT is replaced below, so it must be empty or one this script made (it carries the VERSION
# marker). Anything else, such as ~/Documents, is refused here, before the 437 MB download.
if [ -e "$OUT" ] && [ -n "$(ls -A "$OUT" 2>/dev/null)" ] && [ ! -f "$OUT/VERSION" ]; then
    die "refusing to replace $OUT: it is not empty and was not made by this script (no VERSION marker). Give an empty or new --out."
fi

sha256() {
    if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1; else sha256sum "$1" | cut -d' ' -f1; fi
}

WORK="$(mktemp -d "${TMPDIR:-/tmp}/conductor-pg.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
ZIP="$WORK/postgresql.zip"
echo "fetch-postgres: downloading PostgreSQL $PG_VERSION (about 437 MB)"
curl -fL --retry 3 --retry-delay 5 -o "$ZIP" "$PG_URL"
GOT="$(sha256 "$ZIP")"
[ "$GOT" = "$PG_SHA256" ] || die "sha256 mismatch for $PG_URL: got $GOT, pinned $PG_SHA256. Nothing was unpacked."

PATTERNS=()
for b in "${KEEP_BIN[@]}"; do PATTERNS+=("pgsql/bin/$b"); done
PATTERNS+=('pgsql/lib/*' 'pgsql/share/*')
unzip -q "$ZIP" "${PATTERNS[@]}" \
    -x 'pgsql/lib/*.a' 'pgsql/lib/pkgconfig/*' 'pgsql/lib/postgresql/pgxs/*' 'pgsql/share/man/*' 'pgsql/share/doc/*' \
    -d "$WORK/unpacked"
rm -f "$ZIP"
for b in "${KEEP_BIN[@]}"; do
    [ -f "$WORK/unpacked/pgsql/bin/$b" ] || die "the archive has no bin/$b"
done

# The archive stores each library under every name it has (libicudata.dylib,
# libicudata.68.dylib, libicudata.68.2.dylib: 57 MB, three times). Keep the longest name as
# the file and make the others links to it, which is how an install lays them out.
LIB="$WORK/unpacked/pgsql/lib"
for f in "$LIB"/*.dylib; do
    [ -f "$f" ] && [ ! -L "$f" ] || continue
    printf '%s %s %s\n' "$(sha256 "$f")" "$(basename "$f" | wc -c | tr -d ' ')" "$(basename "$f")"
done | LC_ALL=C sort -k1,1 -k2,2nr | {
    keep_hash="" keep_name=""
    while read -r hash _ name; do
        if [ "$hash" = "$keep_hash" ]; then
            rm -f "$LIB/$name"
            ln -s "$keep_name" "$LIB/$name"
        else
            keep_hash="$hash" keep_name="$name"
        fi
    done
}

chmod -R u+w "$WORK/unpacked/pgsql"
STAGED="$WORK/unpacked/pgsql"

# The checks run on the staged tree, before OUT is touched. A check that fails leaves OUT as
# it was, and no VERSION marker that would make the next run skip the checks.
if [ "$(uname -s)" = "Darwin" ]; then
    ARCHS="$(lipo -archs "$STAGED/bin/postgres")"
    case " $ARCHS " in
        *" arm64 "*" x86_64 "*|*" x86_64 "*" arm64 "*) ;;
        *) die "bin/postgres holds only '$ARCHS'; the app needs arm64 and x86_64" ;;
    esac
    "$STAGED/bin/postgres" --version
else
    echo "fetch-postgres: not on macOS, so the architectures and postgres --version are not checked"
fi

rm -rf "$OUT"
mkdir -p "$(dirname "$OUT")"
mv "$STAGED" "$OUT"
printf '%s %s\n%s\n' "$PG_VERSION" "$PG_SHA256" "$PG_URL" >"$OUT/VERSION"
echo "fetch-postgres: PostgreSQL $PG_VERSION in $OUT ($(du -sh "$OUT" | cut -f1))"
