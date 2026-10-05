#!/usr/bin/env bash
#
# Idempotently manage a conductor PATH entry in the user's shell startup file.
#
#   install-path.sh <dir>            add <dir> to PATH for the user's login shell
#   install-path.sh --remove <dir>   remove every conductor PATH entry this script added
#
# The shell is taken from $SHELL:
#   zsh   ~/.zshrc
#   bash  ~/.bashrc (Linux) or ~/.bash_profile (macOS, whose terminals start login shells)
#   fish  ~/.config/fish/conf.d/conductor.fish
# Any other shell is left untouched, and the exact line to add is printed instead.
#
# In zsh and bash files the entry is wrapped in sentinel comments, so repeated installs are
# no-ops and uninstall is a clean block delete. Override the target file with RCFILE=/path
# (ZSHRC=/path is still honoured for zsh).

set -euo pipefail

BEGIN='# >>> conductor >>>'
END='# <<< conductor <<<'
FISH_FILE="${XDG_CONFIG_HOME:-$HOME/.config}/fish/conf.d/conductor.fish"

resolve() {
  case "$1" in
    /*) printf '%s' "$1" ;;
    *)  printf '%s/%s' "$HOME" "$1" ;;
  esac
}

shell_name() {
  basename "${SHELL:-}"
}

# rc_file prints the startup file for a POSIX-style shell, or nothing for one we do not edit.
rc_file() {
  if [[ -n "${RCFILE:-}" ]]; then
    printf '%s' "$RCFILE"
    return
  fi
  case "$(shell_name)" in
    zsh) printf '%s' "${ZSHRC:-$HOME/.zshrc}" ;;
    bash)
      if [[ "$(uname -s)" == Darwin ]]; then
        printf '%s' "$HOME/.bash_profile"
      else
        printf '%s' "$HOME/.bashrc"
      fi ;;
  esac
}

on_path() {
  case ":$PATH:" in
    *":$1:"*) return 0 ;;
  esac
  return 1
}

add_block() {
  local file="$1" dir="$2"
  mkdir -p "$(dirname "$file")"
  touch "$file"
  if grep -Fq "$BEGIN" "$file"; then
    echo "$dir already on PATH in $file"
    return 0
  fi
  cat >> "$file" <<EOF

$BEGIN
export PATH="\$PATH:$dir"
$END
EOF
  echo "added $dir to PATH in $file"
  echo "open a new terminal, or run: source $file"
}

add() {
  local dir
  dir="$(resolve "$1")"
  local file
  file="$(rc_file)"
  if [[ -n "$file" ]]; then
    add_block "$file" "$dir"
    return 0
  fi
  if [[ "$(shell_name)" == fish ]]; then
    mkdir -p "$(dirname "$FISH_FILE")"
    printf "# Added by conductor (make install); remove with make uninstall.\nfish_add_path -g '%s'\n" "$dir" > "$FISH_FILE"
    echo "added $dir to PATH in $FISH_FILE"
    echo "open a new terminal to pick it up"
    return 0
  fi
  if on_path "$dir"; then
    echo "$dir is already on PATH"
    return 0
  fi
  echo "Your shell ($(shell_name)) is not one this script edits. Add this line to its startup file:"
  echo
  case "$(shell_name)" in
    csh|tcsh) echo "  setenv PATH \"\${PATH}:$dir\"" ;;
    *)        echo "  export PATH=\"\$PATH:$dir\"" ;;
  esac
}

remove_block() {
  local file="$1"
  [[ -f "$file" ]] || return 0
  grep -Fq "$BEGIN" "$file" || return 0
  awk -v b="$BEGIN" -v e="$END" '$0==b{f=1;next} $0==e{f=0;next} !f' "$file" > "$file.tmp"
  mv "$file.tmp" "$file"
  echo "removed conductor PATH entry from $file"
}

# Removal checks every file an install could have written, not just the current shell's:
# people change shells, and a stale PATH entry is harmless only until it is not.
remove() {
  local file
  for file in "${RCFILE:-}" "${ZSHRC:-$HOME/.zshrc}" "$HOME/.bashrc" "$HOME/.bash_profile"; do
    [[ -n "$file" ]] && remove_block "$file"
  done
  if [[ -f "$FISH_FILE" ]]; then
    rm -f "$FISH_FILE"
    echo "removed $FISH_FILE"
  fi
  return 0
}

case "${1:-}" in
  --remove) remove ;;
  ""|-h|--help)
    cat <<'USAGE'
usage: install-path.sh <dir>           # add <dir> to PATH for your shell (zsh, bash, fish)
       install-path.sh --remove <dir>  # remove the conductor PATH entries
USAGE
    exit 1 ;;
  *) add "$1" ;;
esac
