// Package vault stores sessions, tokens and memories encrypted at rest and
// manages the keys that protect them.
//
// # Key hierarchy
//
//	root secret ─┬─ passphrase ──PBKDF2-HMAC-SHA256(salt, iterations)──┐
//	             ├─ 32-byte key file ───────────────────────────────────┤ master key (32 B)
//	             ├─ Shamir-recovered master key ────────────────────────┘
//	             │        │
//	             │        └─HKDF-SHA256(salt=vault id, "gta-vault/v1/root-seal")─► root sealing key
//	             └─ KMS (Wrap/Unwrap) ────────────────────────────────────────────► root sealer
//
//	root sealer ──AES-256-GCM──► keyring { KEK v1..vN (random 32 B), index key (random 32 B) }
//
//	KEK vN ──HKDF-SHA256(salt=vault id, "gta-vault/v1/dek-wrap/kek-N")──► DEK wrapping key
//	DEK (random 32 B per object write) ──AES-256-GCM──► metadata and body sections
//	index key ──HMAC-SHA256──► object id tag (also the object's file name)
//
// KEKs are random rather than derived from the master key, so changing the
// passphrase (Rekey) only re-seals the keyring, and rotating the KEK (Rotate)
// does not require the root secret to change. Old KEK versions stay in the
// keyring, decrypt-only, until Migrate has moved every object to the current
// version and PruneKeys has proven that nothing references them.
//
// # Object envelope, format version 1
//
// All integers are big-endian. Parsing is strict: every length is checked
// against the fixed size for its kind, reserved bytes must be zero and
// trailing bytes are rejected, so an encoded envelope has exactly one valid
// byte representation.
//
//	offset size  field
//	0      4     magic "GTAV"
//	4      1     format version            0x01
//	5      1     cipher suite              0x01 = AES-256-GCM sections, AES-256-GCM DEK wrap, HKDF-SHA256
//	6      1     wrap kind                 0x01 = keyring KEK, 0x02 = hybrid recipient (X25519 + ML-KEM-768)
//	7      1     flags                     must be 0x00
//	8      4     KEK version               >= 1 for wrap kind 0x01, exactly 0 for 0x02
//	12     8     object version            >= 1, incremented on every Put of the same id
//	20     32    object id tag             HMAC-SHA256(index key, label || id) for 0x01,
//	                                       SHA-256(label || id) for 0x02
//	52     16    DEK id                    random, names the DEK
//	68     2     W = wrapped DEK length    60 for 0x01, 1212 for 0x02
//	70     W     wrapped DEK
//	70+W   12    metadata nonce            random
//	82+W   4     M = metadata ciphertext length (incl. 16-byte tag), 16 <= M <= 65552
//	86+W   M     metadata ciphertext       JSON record, includes the plaintext id
//	86+W+M 12    body nonce                random
//	98+W+M 8     B = body ciphertext length (incl. 16-byte tag)
//	106+W+M B    body ciphertext
//	             end of file (anything after is rejected)
//
// Wrapped DEK, wrap kind 0x01 (60 bytes):
//
//	nonce (12) || AES-256-GCM(DEK wrapping key of the KEK version, DEK) (32 + 16 tag)
//
// Wrapped DEK, wrap kind 0x02 (1212 bytes):
//
//	recipient fingerprint (32) || X25519 ephemeral public key (32) ||
//	ML-KEM-768 ciphertext (1088) || nonce (12) || AES-256-GCM(hybrid key, DEK) (48)
//
// Associated data:
//
//	section AAD  = "gta-vault/v1/section" || "GTAV" || 0x01 || 0x01 || section ('M' or 'B') ||
//	               object version || object id tag || DEK id
//	DEK-wrap AAD = "gta-vault/v1/dek-wrap" || "GTAV" || 0x01 || 0x01 || wrap kind || flags ||
//	               KEK version || object version || object id tag || DEK id
//
// The sections bind the object identity, its version and the DEK id; the
// wrapped DEK additionally binds the KEK version. Because the sections do not
// bind the KEK version or the wrapped DEK, a DEK can be re-wrapped under a new
// KEK (lazily on Get, or by Migrate) without touching the ciphertext; any
// substitution of a wrapped DEK from another object, version or KEK fails the
// DEK unwrap. Every byte of an envelope is covered either by strict parsing or
// by a GCM tag; the tests flip every byte to check this.
//
// Each DEK encrypts exactly two messages (metadata and body) with independent
// random 96-bit nonces, far below the GCM random-nonce bound.
//
// # Keyring plaintext
//
//	"GTKR" || 0x01 || generation (8) || current KEK version (4) || index key (32) ||
//	count (2) || count × { version (4) || created unix seconds (8) || KEK (32) }
//
// versions strictly ascending; the whole record is sealed by the root sealer
// with AAD = "gta-vault/v1/keyring" || vault id || canonical root parameters,
// so KDF parameters cannot be swapped without detection.
//
// # On-disk layout
//
//	<dir>/            0700
//	  vault.json      0600  vault id, root parameters (KDF salt, iterations, KMS key id), sealed keyring
//	  objects/        0700
//	    <hex tag>.gtav 0600 one envelope per object
//
// Every write goes to a temporary file in the same directory, is fsynced,
// renamed over the target and followed by an fsync of the directory.
//
// # Threat model and limitations
//
// The vault protects confidentiality and integrity of data at rest against
// anyone who can read or modify the directory but does not hold the root
// secret. It does not protect against a compromised running process. It does
// not detect whole-object rollback (an attacker replacing an object with an
// older, validly encrypted version of the same id); callers that need that can
// compare Info.Version against a trusted counter. Cross-process writers are
// safe against torn files (atomic rename) but administrative operations
// (Rotate, Rekey, PruneKeys) assume a single process owns the vault.
//
// Zeroization is best effort. Key material that this package allocates is
// overwritten on Lock and after use, but Go may have copied it during
// garbage collection or stack growth, crypto/aes keeps expanded key schedules
// in memory owned by the standard library until they are collected, and
// crypto/pbkdf2 accepts the passphrase as a string (the package avoids a copy
// by aliasing the caller's buffer for the duration of the call). The caller
// owns passphrase and key buffers passed to Passphrase and MasterKey and should
// wipe them after the call that consumed them returns.
package vault
