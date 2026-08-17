package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/adamburan/conductor/internal/domain"
)

// Session capability declaration for `conductor wrap` (DESIGN.md §7.3).
//
// A session says what it is running; the control plane resolves that against the model
// catalog to decide what it is worth. Declaring nothing is fine and stays the default: the
// session is still visible in presence, it simply cannot be selected for work that names a
// tier or effort floor.

// parseWrapFlags pulls Conductor's own flags off the front of a wrap invocation and returns
// the remainder untouched.
//
// It stops at the first argument that is not one of ours, rather than using the flag package,
// because everything after the harness name is the harness's own command line and must reach
// it byte for byte.
func parseWrapFlags(args []string) (domain.SessionCapabilities, []string) {
	var caps domain.SessionCapabilities
	for len(args) > 0 {
		arg := args[0]
		name, value, inline := strings.Cut(arg, "=")
		take := func() (string, bool) {
			if inline {
				args = args[1:]
				return value, true
			}
			if len(args) < 2 {
				return "", false
			}
			v := args[1]
			args = args[2:]
			return v, true
		}

		var ok bool
		switch name {
		case "--model", "-model":
			caps.Model, ok = take()
		case "--effort", "-effort":
			var v string
			if v, ok = take(); ok {
				caps.Effort = domain.Effort(v)
			}
		case "--max-effort", "-max-effort":
			var v string
			if v, ok = take(); ok {
				caps.MaxEffort = domain.Effort(v)
			}
		case "--alias", "-alias":
			caps.Alias, ok = take()
		case "--role", "-role":
			var v string
			if v, ok = take(); ok {
				caps.Roles = append(caps.Roles, v)
			}
		default:
			return caps, args
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "conductor wrap: %s needs a value\n", name)
			return caps, args
		}
	}
	return caps, args
}

// detectCapabilities fills gaps in what the user declared from the harness command line and
// the environment.
//
// `conductor wrap claude --model claude-opus-5` is the shape people actually type, so reading
// the model out of the harness's own flags is worth doing: the alternative is a session that
// is demonstrably running a frontier model and advertises nothing.
func detectCapabilities(declared domain.SessionCapabilities, harnessArgs []string) domain.SessionCapabilities {
	caps := declared
	if caps.Model == "" {
		caps.Model = flagValue(harnessArgs, "--model", "-m")
	}
	if caps.Model == "" {
		caps.Model = firstNonEmptyString(
			os.Getenv("CONDUCTOR_MODEL"), os.Getenv("ANTHROPIC_MODEL"), os.Getenv("OPENAI_MODEL"))
	}
	if caps.Effort == "" {
		if v := flagValue(harnessArgs, "--effort", "--reasoning-effort"); v != "" {
			caps.Effort = domain.Effort(v)
		} else if v := os.Getenv("CONDUCTOR_EFFORT"); v != "" {
			caps.Effort = domain.Effort(v)
		}
	}
	// A declared ceiling below the running effort is a contradiction; the running effort is
	// the observable fact, so it wins.
	if caps.MaxEffort != "" && caps.Effort != "" && !caps.MaxEffort.AtLeast(caps.Effort) {
		caps.MaxEffort = caps.Effort
	}
	return caps
}

// flagValue finds `--name value` or `--name=value` among a harness's arguments.
func flagValue(args []string, names ...string) string {
	for i, arg := range args {
		for _, name := range names {
			if arg == name && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(arg, name+"="); ok {
				return v
			}
		}
	}
	return ""
}

// printCapabilityNotice tells the user what the server made of their declaration.
//
// The unresolved case is the one worth reporting: a model the catalog has never heard of
// looks fine locally and then silently loses every selection that names a tier.
func printCapabilityNotice(session domain.Session) {
	caps := session.Capabilities
	if caps.Model == "" && caps.Effort == "" {
		return
	}
	line := fmt.Sprintf("  declared %s", orDash(caps.Model))
	if caps.Effort != "" {
		line += fmt.Sprintf(" at effort %s", caps.Effort)
	}
	if ceiling := caps.Ceiling(); ceiling != caps.Effort && ceiling != "" {
		line += fmt.Sprintf(" (up to %s)", ceiling)
	}
	if caps.Resolved {
		line += fmt.Sprintf(" — tier %s", caps.Tier)
	} else {
		line += " — not in the model catalog, so tier-gated work will not be offered here"
	}
	fmt.Fprintln(os.Stderr, line)
}
