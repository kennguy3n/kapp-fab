package runtime

import "github.com/google/uuid"

// Encryptor is the per-tenant column-encryption interface the runtime
// uses to protect at-rest secrets on marketplace_extension_installations.
//
// At-rest scope: today this covers signing_secret (the HMAC key used
// to sign requests TO the extension's webhook). Devin Review
// ANALYSIS_0004 on PR #128 flagged that the kapp-backup dump path
// uses row_to_json which captures every column verbatim, so any
// secret in the column ends up in plaintext in the backup file —
// making the backup blob a high-value target for an attacker who
// compromises the storage. Encrypting at the column level fixes
// both attack surfaces with one change: the DB row carries
// ciphertext, and the backup file inherits that ciphertext without
// kapp-backup needing to know which columns are sensitive.
//
// The interface is declared inline here, with no dependency on
// internal/tenant, so the runtime package can stay independent of
// the HKDF derivation machinery. The production wiring in
// services/api/deps_build.go (and the future B6 marketplace API
// surface) is expected to pass a `*tenant.KeyManager`, which
// satisfies this interface naturally (see
// internal/tenant/encryption.go:KeyManager.EncryptString /
// DecryptString — same signatures).
//
// When the Engine and Dispatcher are constructed with a nil
// Encryptor (the dev-mode path with KAPP_MASTER_KEY unset), all
// operations degrade to plaintext: EncryptString returns the input,
// DecryptString returns the input. The fallback ONLY applies to the
// nil-Encryptor case at construction — once an encryptor is wired,
// it MUST round-trip every value. We don't auto-detect the prefix
// here because that is the tenant.KeyManager's responsibility on
// the decrypt side (it already returns prefix-less values verbatim),
// and the engine's own ciphertext-vs-plaintext detection belongs
// in the integration paths, not in this shim.
type Encryptor interface {
	EncryptString(tenantID uuid.UUID, plaintext string) (string, error)
	DecryptString(tenantID uuid.UUID, value string) (string, error)
}

// noopEncryptor is the zero-config Encryptor: every value passes
// through unchanged. Used internally when EngineOptions.Encryptor
// (or the equivalent on Dispatcher) is nil so the call sites can
// always call into a non-nil Encryptor and avoid per-call nil
// branching.
type noopEncryptor struct{}

// EncryptString returns plaintext unchanged. noopEncryptor is the
// fallback for the dev-mode KAPP_MASTER_KEY-unset path; tagging it
// "encrypt" preserves a consistent call shape with the production
// *tenant.KeyManager.
func (noopEncryptor) EncryptString(_ uuid.UUID, plaintext string) (string, error) {
	return plaintext, nil
}

// DecryptString returns value unchanged. Symmetric to EncryptString
// — the dev-mode path stores plaintext in the column and reads it
// back without transformation.
func (noopEncryptor) DecryptString(_ uuid.UUID, value string) (string, error) {
	return value, nil
}

// resolveEncryptor returns enc if non-nil, otherwise the noop
// encryptor. Centralising the nil-fallback so engine.go and
// dispatcher.go don't reinvent it.
func resolveEncryptor(enc Encryptor) Encryptor {
	if enc == nil {
		return noopEncryptor{}
	}
	return enc
}
