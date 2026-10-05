package main

import (
	"flag"
	"fmt"

	"github.com/adamburan/conductor/internal/version"
)

// cmdVersion prints the build this CLI is. `conductor doctor` compares it with the server's.
func cmdVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *asJSON {
		return emit(version.Get())
	}
	fmt.Println("conductor", version.Get())
	return nil
}
