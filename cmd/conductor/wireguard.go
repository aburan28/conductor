package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/aburan28/conductor/internal/wireguard"
)

func cmdWireGuard(args []string) error {
	if len(args) == 0 || args[0] != "render" {
		return errors.New("usage: conductor wireguard render --manifest mesh.yaml --out NEW_DIRECTORY")
	}
	fs := flag.NewFlagSet("wireguard render", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "versioned public WireGuard network manifest (YAML)")
	output := fs.String("out", "", "new owner-only output directory; existing paths are refused")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *manifestPath == "" || *output == "" || fs.NArg() != 0 {
		return errors.New("usage: conductor wireguard render --manifest mesh.yaml --out NEW_DIRECTORY")
	}
	if err := renderWireGuardFiles(*manifestPath, *output); err != nil {
		return err
	}
	fmt.Printf("WireGuard templates written to %s; add each private key on its owning node before activation.\n", *output)
	return nil
}

func renderWireGuardFiles(manifestPath, output string) error {
	input, err := os.Open(manifestPath)
	if err != nil {
		return err
	}
	defer input.Close()
	manifest, err := wireguard.Load(input)
	if err != nil {
		return err
	}
	files, err := manifest.Render()
	if err != nil {
		return err
	}
	// Creating a fresh directory prevents replacement of configurations that may
	// already contain a locally inserted private key, including symlink targets.
	if err := os.Mkdir(output, 0700); err != nil {
		return fmt.Errorf("create new WireGuard output directory: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(output) // only the newly created directory belongs to us
		}
	}()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file, err := os.OpenFile(filepath.Join(output, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(files[name])
		closeErr := file.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return err
		}
	}
	complete = true
	return nil
}
