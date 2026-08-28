#!/usr/bin/env bash
#
# Idempotently manage a conductor PATH entry in ~/.zshrc.
#
#   install-path.sh <dir>            add <dir> to PATH in ~/.zshrc
#   install-path.sh --remove <dir>    remove the conductor PATH entry
#
# The entry is wrapped in sentinel comments so repeated installs are no-ops
# and uninstall is a clean block delete. Override the target file with
# ZSHRC=/path/to/zshrc.

set -euo pipefail

ZSHRC="${ZSHRC:-$HOME/.zshrc}"
BEGIN='# >>> conductor >>>'
END='# <<< conductor <<<'

resolve() {
  case "$1" in
    /*) printf '%s' "$1" ;;
    *)  printf '%s/%s' "$HOME" "$1" ;;
  esac
}

add() {
  local dir
  dir="$(resolve "$1")"
  mkdir -p "$(dirname "$ZSHRC")"
  touch "$ZSHRC"
  if grep -Fq "$BEGIN" "$ZSHRC"; then
    echo "$dir already on PATH in $ZSHRC"
    return 0
  fi
  cat >> "$ZSHRC" <<EOF

$BEGIN
export PATH="\$PATH:$dir"
$END
EOF
  echo "added $dir to PATH in $ZSHRC"
  echo "restart your shell, or run: source $ZSHRC"
}

remove() {
  [[ -f "$ZSHRC" ]] || { echo "$ZSHRC not found; nothing to remove"; return 0; }
  if ! grep -Fq "$BEGIN" "$ZSHRC"; then
    echo "no conductor PATH entry in $ZSHRC"
    return 0
  fi
  awk -v b="$BEGIN" -v e="$END" '$0==b{f=1;next} $0==e{f=0;next} !f' "$ZSHRC" > "$ZSHRC.tmp"
  mv "$ZSHRC.tmp" "$ZSHRC"
  echo "removed conductor PATH entry from $ZSHRC"
}

case "${1:-}" in
  --remove) remove "${2:-}" ;;
  ""|-h|--help)
    cat <<'USAGE'
usage: install-path.sh <dir>           # add <dir> to PATH in ~/.zshrc
       install-path.sh --remove <dir>  # remove the conductor PATH entry
USAGE
    exit 1 ;;
  *) add "$1" ;;
esac
