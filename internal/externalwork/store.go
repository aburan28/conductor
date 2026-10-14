package externalwork

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// InventoryPath is owner-local state, separate from Conductor's server sessions.
func InventoryPath(home, stateDir string) string {
	if stateDir == "" {
		stateDir = filepath.Join(home, ".conductor")
	}
	return filepath.Join(stateDir, "external-work", "latest.json")
}

// SaveLatest replaces only the derived inventory, atomically and with owner-only modes.
// Source transcripts and checkpoints are never modified.
func SaveLatest(path string, inv Inventory) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".inventory-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(inv); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func LoadLatest(path string) (Inventory, error) {
	f, err := os.Open(path)
	if err != nil {
		return Inventory{}, err
	}
	defer f.Close()
	var inv Inventory
	if err := json.NewDecoder(f).Decode(&inv); err != nil {
		return Inventory{}, fmt.Errorf("read external work inventory: %w", err)
	}
	if inv.Version != 1 {
		return Inventory{}, errors.New("unsupported external work inventory version")
	}
	return inv, nil
}
