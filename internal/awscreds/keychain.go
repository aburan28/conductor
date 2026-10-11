package awscreds

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/aburan28/conductor/internal/backup"
)

// Keychain service names shared with the macOS app (docs/STORAGE.md).
const (
	KeychainS3Service   = "dev.conductor.s3"
	KeychainSealService = "dev.conductor.seal"
	KeychainSealAccount = "default"
)

// securityTool is the macOS command-line front end to the Keychain. The macOS app lists it
// among each item's trusted applications, so reading through it does not prompt.
const securityTool = "/usr/bin/security"

// ErrNoKeychain is returned off macOS.
var ErrNoKeychain = errors.New("the Keychain is only available on macOS")

// ErrKeychainItemNotFound is returned when the item does not exist.
var ErrKeychainItemNotFound = errors.New("not found in the Keychain")

// KeychainGet reads a generic password.
func KeychainGet(ctx context.Context, env Env, service, account string) (string, error) {
	if env.GOOS != "darwin" {
		return "", ErrNoKeychain
	}
	out, err := env.Command(ctx, nil, securityTool, "find-generic-password", "-s", service, "-a", account, "-w")
	if err != nil {
		// security exits 44 for "The specified item could not be found in the keychain."
		if strings.Contains(err.Error(), "exit status 44") || strings.Contains(err.Error(), "could not be found") {
			return "", fmt.Errorf("%s/%s: %w", service, account, ErrKeychainItemNotFound)
		}
		return "", fmt.Errorf("reading %s/%s from the Keychain: %w", service, account, err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// KeychainSet stores (or replaces) a generic password. The secret goes to `security -i` on
// standard input, never on a command line where `ps` could see it.
func KeychainSet(ctx context.Context, env Env, service, account, secret string) error {
	if env.GOOS != "darwin" {
		return ErrNoKeychain
	}
	if strings.ContainsAny(secret, "\r\n") {
		return errors.New("the secret contains a line break")
	}
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", quoteSecurity(service), quoteSecurity(account), quoteSecurity(secret))
	if _, err := env.Command(ctx, []byte(line), securityTool, "-i"); err != nil {
		return fmt.Errorf("storing %s/%s in the Keychain: %w", service, account, err)
	}
	return nil
}

// KeychainDelete removes a generic password; a missing item is not an error.
func KeychainDelete(ctx context.Context, env Env, service, account string) error {
	if env.GOOS != "darwin" {
		return ErrNoKeychain
	}
	if _, err := env.Command(ctx, nil, securityTool, "delete-generic-password", "-s", service, "-a", account); err != nil {
		if strings.Contains(err.Error(), "exit status 44") || strings.Contains(err.Error(), "could not be found") {
			return nil
		}
		return fmt.Errorf("removing %s/%s from the Keychain: %w", service, account, err)
	}
	return nil
}

// quoteSecurity quotes one argument for `security -i`, which splits its input lines like a
// shell: double quotes, with backslash escaping a quote or a backslash.
func quoteSecurity(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// StaticKeychain is the `static` method with the secret in the Keychain. The secret is read
// on first use and held in memory for the life of the process; a failed read is retried.
func StaticKeychain(env Env, accessKeyID string) Provider {
	var (
		mu     sync.Mutex
		secret string
	)
	return ProviderFunc(func(ctx context.Context) (backup.Credentials, error) {
		mu.Lock()
		defer mu.Unlock()
		if secret == "" {
			s, err := KeychainGet(ctx, env, KeychainS3Service, accessKeyID)
			if err != nil {
				return backup.Credentials{}, fmt.Errorf("the secret for access key %s: %w", accessKeyID, err)
			}
			secret = s
		}
		return backup.Credentials{AccessKey: accessKeyID, SecretKey: secret, Source: "access key (Keychain)"}, nil
	})
}
