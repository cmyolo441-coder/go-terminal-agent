package vault

import (
	"errors"
	"fmt"
	"runtime"
)

// Sentinel errors. Errors returned by this package wrap one of these where it
// applies, so callers should test with errors.Is.
var (
	// ErrLocked is returned by operations that need keys while the vault is locked.
	ErrLocked = errors.New("vault: locked")
	// ErrNotFound means no object exists for the id.
	ErrNotFound = errors.New("vault: object not found")
	// ErrExists means a vault, key file or object already exists where a new one was requested.
	ErrExists = errors.New("vault: already exists")
	// ErrWrongKey means the root secret did not authenticate the keyring.
	ErrWrongKey = errors.New("vault: wrong key")
	// ErrCorrupt means data failed strict parsing or authentication.
	ErrCorrupt = errors.New("vault: malformed or tampered data")
	// ErrInvalidID means an object id is empty, too long or contains control characters.
	ErrInvalidID = errors.New("vault: invalid object id")
	// ErrInvalidArgument reports a parameter outside its documented range.
	ErrInvalidArgument = errors.New("vault: invalid argument")
	// ErrUnknownKeyVersion means an object references a KEK version the keyring does not hold.
	ErrUnknownKeyVersion = errors.New("vault: unknown key version")
	// ErrWrongRecipient means a shared envelope was not addressed to the given identity.
	ErrWrongRecipient = errors.New("vault: envelope is for a different recipient")
	// ErrShareIntegrity means a secret share is damaged, from another set, or inconsistent.
	ErrShareIntegrity = errors.New("vault: share integrity check failed")
	// ErrNotEnoughShares means fewer shares than the threshold were supplied.
	ErrNotEnoughShares = errors.New("vault: not enough shares")
	// ErrInsecurePermissions means a vault directory or key file is accessible to other users.
	ErrInsecurePermissions = errors.New("vault: insecure file permissions")
	// ErrTooLarge means an object or field exceeds its size limit.
	ErrTooLarge = errors.New("vault: too large")
	// ErrKeySourceMismatch means the key source kind does not fit the vault's root kind.
	ErrKeySourceMismatch = errors.New("vault: key source does not match vault")
)

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// wipe overwrites b with zeros. It is best effort; see the package
// documentation for what Go's runtime may have copied elsewhere.
func wipe(b []byte) {
	clear(b)
	runtime.KeepAlive(b)
}

// wipeAll wipes each slice.
func wipeAll(bs ...[]byte) {
	for _, b := range bs {
		wipe(b)
	}
}
