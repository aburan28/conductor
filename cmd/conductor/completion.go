package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// cmdCompletion prints a static completion script for a shell. Static on purpose: completing
// task refs or handles would mean a network call on every <Tab>, and a completion that hangs
// on a slow control plane is worse than none. Commands and subcommands come from the same
// index as the help, so they cannot drift apart.
func cmdCompletion(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: conductor completion bash|zsh|fish")
	}
	return writeCompletion(os.Stdout, args[0])
}

func writeCompletion(w io.Writer, shell string) error {
	switch shell {
	case "bash":
		writeBashCompletion(w)
	case "zsh":
		// zsh runs bash completion functions through bashcompinit, which is simpler than a
		// second hand-written grammar and completes exactly the same words.
		fmt.Fprint(w, "#compdef conductor\nautoload -U +X bashcompinit && bashcompinit\n")
		writeBashCompletion(w)
	case "fish":
		writeFishCompletion(w)
	default:
		return fmt.Errorf("unsupported shell %q (want bash, zsh, or fish)", shell)
	}
	return nil
}

func writeBashCompletion(w io.Writer) {
	fmt.Fprintf(w, `# conductor bash completion. Load with:  source <(conductor completion bash)
_conductor() {
  local cur=${COMP_WORDS[COMP_CWORD]}
  if [[ $COMP_CWORD -eq 1 ]]; then
    COMPREPLY=($(compgen -W %q -- "$cur"))
    return
  fi
  if [[ $COMP_CWORD -eq 2 ]]; then
    local subs=""
    case ${COMP_WORDS[1]} in
`, strings.Join(commandNames(), " "))
	for _, c := range commands {
		if len(c.subs) > 0 {
			fmt.Fprintf(w, "      %s) subs=%q ;;\n", c.name, strings.Join(c.subs, " "))
		}
	}
	fmt.Fprintf(w, "      help) subs=%q ;;\n", strings.Join(append(topicNames(), "all"), " "))
	fmt.Fprint(w, `    esac
    COMPREPLY=($(compgen -W "$subs -h" -- "$cur"))
    return
  fi
  if [[ $COMP_CWORD -eq 3 ]]; then
    local nested=""
    case "${COMP_WORDS[1]} ${COMP_WORDS[2]}" in
`)
	for _, c := range commands {
		for _, sub := range sortedKeys(c.nested) {
			fmt.Fprintf(w, "      %q) nested=%q ;;\n", c.name+" "+sub, strings.Join(c.nested[sub], " "))
		}
	}
	fmt.Fprint(w, `    esac
    if [[ -n $nested ]]; then
      COMPREPLY=($(compgen -W "$nested -h" -- "$cur"))
      return
    fi
  fi
  COMPREPLY=($(compgen -f -- "$cur"))
}
complete -o default -F _conductor conductor
`)
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func writeFishCompletion(w io.Writer) {
	fmt.Fprintln(w, "# conductor fish completion. Install with:  conductor completion fish > ~/.config/fish/completions/conductor.fish")
	for _, c := range commands {
		fmt.Fprintf(w, "complete -c conductor -f -n __fish_use_subcommand -a %s -d %s\n", c.name, fishQuote(c.summary))
		for _, sub := range c.subs {
			if _, nested := c.nested[sub]; nested {
				// Offered only before the subcommand is typed, so its own words come next.
				fmt.Fprintf(w, "complete -c conductor -f -n '__fish_seen_subcommand_from %s; and not __fish_seen_subcommand_from %s' -a %s\n", c.name, sub, sub)
				continue
			}
			fmt.Fprintf(w, "complete -c conductor -f -n '__fish_seen_subcommand_from %s' -a %s\n", c.name, sub)
		}
		for _, sub := range sortedKeys(c.nested) {
			for _, word := range c.nested[sub] {
				fmt.Fprintf(w, "complete -c conductor -f -n '__fish_seen_subcommand_from %s; and __fish_seen_subcommand_from %s' -a %s\n", c.name, sub, word)
			}
		}
	}
	fmt.Fprintln(w, "complete -c conductor -f -n __fish_use_subcommand -a help -d 'help for a command'")
	fmt.Fprintf(w, "complete -c conductor -f -n '__fish_seen_subcommand_from help' -a '%s all'\n", strings.Join(topicNames(), " "))
}

func fishQuote(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}
