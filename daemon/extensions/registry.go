package extensions

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

var (
	ErrNotFound           = errors.New("extensions: not found")
	ErrAlreadyExists      = errors.New("extensions: already discovered at this id/version")
	ErrInvalidState       = errors.New("extensions: not in a valid state for this transition")
	ErrMissingRequirement = errors.New("extensions: a required capability is not provided by any active extension")
	ErrActiveDependents   = errors.New("extensions: other active extensions still require a capability this extension provides")
	ErrIncompatibleCore   = errors.New("extensions: manifest's spec.compatibility.amhCore range does not admit this daemon's core version")
	ErrSignatureRequired  = errors.New("extensions: this registry requires every manifest to declare a valid spec.signature")
	ErrInvalidSignature   = errors.New("extensions: spec.signature failed verification")
)

// CoreVersion is this daemon build's own compatibility version — what a
// manifest's spec.compatibility.amhCore range is checked against. It is
// not the AMH specification's version; it is the version a domain
// extension author targets when declaring "what core am I compatible
// with," the qualification half of §14's "signed extension packs and
// compatibility qualification."
const CoreVersion = "0.1.0"

// Status is the extension lifecycle state — the Cordis activation pipeline:
// discovered -> activating -> active -> quiescing -> disposed, with
// activating able to fail into "failed" (auto-rolled-back, retryable).
type Status string

const (
	StatusDiscovered Status = "discovered"
	StatusActivating Status = "activating"
	StatusActive     Status = "active"
	StatusQuiescing  Status = "quiescing"
	StatusDisposed   Status = "disposed"
	StatusFailed     Status = "failed"
)

// Extension is one row of the registry, as returned to callers (the API
// layer, tests, and eventually a UI extension listing installed modules).
type Extension struct {
	ID             string
	Version        string
	Name           string
	Publisher      string
	Description    string
	Entrypoint     string
	Isolation      Isolation
	Provides       []CapabilityRef
	Requires       []Requirement
	Status         Status
	StatusReason   string
	RuntimeHandle  string
	ManifestDigest string
}

// Registry is the durable, restart-surviving Cordis-lifecycle extension
// registry: knowledge-base, memory, model-provider, connector, harness,
// and user-surface extensions are all just extensions here — the registry
// has no domain-specific knowledge of what any of them do, only of their
// declared capabilities and dependencies. That domain-neutrality is what
// makes this the actual seam the v10 spec calls "small hard core,
// reversible extensions" — this package IS the hard core's extension host
// (§2.1: "extension discovery, dependency resolution, activation,
// quiescence, disposal, and rollback").
type Registry struct {
	DB *sql.DB
	l  *launcher

	// Trust is the operator-managed set of Ed25519 keys Discover trusts to
	// admit a signed manifest (see trust.go).
	Trust *TrustStore

	// RequireSignatures, when true, fails Discover closed for any manifest
	// that declares no spec.signature at all — see Discover's doc comment
	// and README's "What's declared but not yet built" for why this
	// defaults to false rather than being mandatory from day one.
	RequireSignatures bool
}

func New(db *sql.DB) *Registry {
	return &Registry{DB: db, l: newLauncher(), Trust: newTrustStore(db)}
}

// Discover validates a manifest and records it as a candidate extension —
// not yet active, not yet touching any process or container. Re-Discovering
// the same id/version with an unchanged manifest digest is idempotent;
// with a changed digest it is refused (bump the version instead — this
// registry never silently swaps out a running extension's declared
// contract).
//
// Two more admission checks run here, both real and both fail-closed where
// they apply (§14's "signed extension packs and compatibility
// qualification"):
//
//   - compatibility: m.Spec.Compatibility.AMHCore is always checked against
//     this build's CoreVersion — a manifest declaring a range this daemon
//     doesn't satisfy is refused unconditionally, the same way a capability
//     requirement's version range is enforced during Activate.
//   - signature: if a manifest declares spec.signature, it MUST verify —
//     algorithm must be ed25519, the declared digest must equal the
//     manifest's own recomputed SignableDigest (never trust the caller's
//     digest), the keyId must resolve to a currently-trusted, non-revoked
//     key in r.Trust, and the Ed25519 signature must verify against that
//     key. If a manifest declares no signature at all, Discover admits it
//     exactly as before UNLESS r.RequireSignatures is set — the same
//     "fail-closed only where a property is actually declared" posture
//     daemon/policy takes toward reversibility, not a claim that every
//     manifest in this registry is signed today.
func (r *Registry) Discover(ctx context.Context, m Manifest) (*Extension, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := r.checkCompatibility(m); err != nil {
		return nil, err
	}
	if err := r.checkSignature(ctx, m); err != nil {
		return nil, err
	}
	digest, err := m.Digest()
	if err != nil {
		return nil, err
	}

	existing, err := r.Get(ctx, m.Metadata.ID, m.Metadata.Version)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if err == nil {
		if existing.ManifestDigest == digest {
			return existing, nil
		}
		return nil, fmt.Errorf("%w: %s@%s already discovered with a different manifest", ErrAlreadyExists, m.Metadata.ID, m.Metadata.Version)
	}

	providesJSON, err := json.Marshal(m.Spec.Provides)
	if err != nil {
		return nil, fmt.Errorf("extensions: marshal provides: %w", err)
	}
	requiresJSON, err := json.Marshal(m.Spec.Requires)
	if err != nil {
		return nil, fmt.Errorf("extensions: marshal requires: %w", err)
	}
	actionsJSON, err := json.Marshal(m.Spec.Actions)
	if err != nil {
		return nil, fmt.Errorf("extensions: marshal actions: %w", err)
	}
	compatJSON, err := json.Marshal(m.Spec.Compatibility)
	if err != nil {
		return nil, fmt.Errorf("extensions: marshal compatibility: %w", err)
	}
	var sigJSON any
	if m.Spec.Signature != nil {
		b, err := json.Marshal(m.Spec.Signature)
		if err != nil {
			return nil, fmt.Errorf("extensions: marshal signature: %w", err)
		}
		sigJSON = string(b)
	}

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("extensions: begin discover tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO extension (id, version, name, publisher, description, entrypoint, isolation, provides, requires, actions, compatibility, signature, manifest_digest, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'discovered')`,
		m.Metadata.ID, m.Metadata.Version, m.Metadata.Name, m.Metadata.Publisher, m.Metadata.Description,
		m.Spec.Entrypoint, string(m.Spec.Isolation), string(providesJSON), string(requiresJSON), string(actionsJSON), string(compatJSON), sigJSON, digest,
	)
	if err != nil {
		return nil, fmt.Errorf("extensions: insert extension: %w", err)
	}
	for _, p := range m.Spec.Provides {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO extension_provided_capability (extension_id, extension_version, capability_id, capability_version, contract)
			VALUES ($1, $2, $3, $4, $5)`,
			m.Metadata.ID, m.Metadata.Version, p.ID, p.Version, p.Contract,
		); err != nil {
			return nil, fmt.Errorf("extensions: insert provided capability %s: %w", p.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("extensions: commit discover: %w", err)
	}
	return r.Get(ctx, m.Metadata.ID, m.Metadata.Version)
}

// Activate resolves spatial composability (every non-optional requirement
// must be provided by some currently-active extension, within the
// declared version range) and, only if resolution succeeds, launches the
// extension per its isolation and records activation as a durable effect.
// A launch failure rolls the extension back to "failed" — never left
// half-active.
func (r *Registry) Activate(ctx context.Context, id, version string) (*Extension, error) {
	ext, err := r.Get(ctx, id, version)
	if err != nil {
		return nil, err
	}
	if ext.Status != StatusDiscovered && ext.Status != StatusDisposed && ext.Status != StatusFailed {
		return nil, fmt.Errorf("%w: %s@%s is %s, not discovered/disposed/failed", ErrInvalidState, id, version, ext.Status)
	}

	for _, req := range ext.Requires {
		if req.Optional {
			continue
		}
		satisfied, err := r.requirementSatisfied(ctx, req)
		if err != nil {
			return nil, err
		}
		if !satisfied {
			return nil, fmt.Errorf("%w: %s requires %s@%s", ErrMissingRequirement, id, req.Capability, req.VersionRange)
		}
	}

	if err := r.setStatus(ctx, id, version, StatusActivating, ""); err != nil {
		return nil, err
	}

	spec := Spec{Entrypoint: ext.Entrypoint, Isolation: ext.Isolation}
	runtimeHandle, launchErr := r.l.launch(ctx, id, version, spec)
	if launchErr != nil {
		_ = r.setStatus(ctx, id, version, StatusFailed, launchErr.Error())
		_ = r.recordEffect(ctx, id, version, "activate", map[string]string{"isolation": string(ext.Isolation)}, nil, "failed")
		return nil, fmt.Errorf("extensions: activate %s@%s: %w", id, version, launchErr)
	}

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		_ = r.l.teardown(ctx, id, version, runtimeHandle)
		return nil, fmt.Errorf("extensions: begin activate-commit tx: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE extension SET status = 'active', runtime_handle = $1, activated_at = iso8601_now(), status_reason = NULL
		WHERE id = $2 AND version = $3`, runtimeHandle, id, version)
	if err != nil {
		tx.Rollback()
		_ = r.l.teardown(ctx, id, version, runtimeHandle)
		return nil, fmt.Errorf("extensions: mark active: %w", err)
	}
	forward, _ := json.Marshal(map[string]string{"isolation": string(ext.Isolation), "runtime_handle": runtimeHandle})
	inverse, _ := json.Marshal(map[string]string{"action": "dispose", "runtime_handle": runtimeHandle})
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO extension_effect (id, extension_id, extension_version, effect_type, forward_payload, inverse_payload, outcome)
		VALUES ($1, $2, $3, 'activate', $4, $5, 'success')`,
		uuid.NewString(), id, version, string(forward), string(inverse),
	); err != nil {
		tx.Rollback()
		_ = r.l.teardown(ctx, id, version, runtimeHandle)
		return nil, fmt.Errorf("extensions: record activate effect: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = r.l.teardown(ctx, id, version, runtimeHandle)
		return nil, fmt.Errorf("extensions: commit activate: %w", err)
	}

	return r.Get(ctx, id, version)
}

// Quiesce is the checkpoint before Dispose: it refuses to proceed while
// another active extension still non-optionally requires a capability
// this one provides (spatial composability governing teardown order — you
// cannot remove a provider out from under an active dependent).
func (r *Registry) Quiesce(ctx context.Context, id, version string) (*Extension, error) {
	ext, err := r.Get(ctx, id, version)
	if err != nil {
		return nil, err
	}
	if ext.Status != StatusActive {
		return nil, fmt.Errorf("%w: %s@%s is %s, not active", ErrInvalidState, id, version, ext.Status)
	}

	for _, p := range ext.Provides {
		dependents, err := r.activeDependents(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		for _, dep := range dependents {
			if dep.id == id && dep.version == version {
				continue
			}
			return nil, fmt.Errorf("%w: %s@%s (requires %s)", ErrActiveDependents, dep.id, dep.version, p.ID)
		}
	}

	if err := r.setStatus(ctx, id, version, StatusQuiescing, ""); err != nil {
		return nil, err
	}
	return r.Get(ctx, id, version)
}

// Dispose tears down a quiescing extension and records disposal as the
// verified inverse of its activation — the property that makes an
// extension a reversible module, not just a deletable row.
func (r *Registry) Dispose(ctx context.Context, id, version string) (*Extension, error) {
	ext, err := r.Get(ctx, id, version)
	if err != nil {
		return nil, err
	}
	if ext.Status != StatusQuiescing {
		return nil, fmt.Errorf("%w: %s@%s is %s, not quiescing (call Quiesce first)", ErrInvalidState, id, version, ext.Status)
	}

	if err := r.l.teardown(ctx, id, version, ext.RuntimeHandle); err != nil {
		_ = r.setStatus(ctx, id, version, StatusFailed, err.Error())
		_ = r.recordEffect(ctx, id, version, "dispose", map[string]string{"runtime_handle": ext.RuntimeHandle}, nil, "failed")
		return nil, fmt.Errorf("extensions: dispose %s@%s: %w", id, version, err)
	}

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("extensions: begin dispose-commit tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE extension SET status = 'disposed', disposed_at = iso8601_now()
		WHERE id = $1 AND version = $2`, id, version,
	); err != nil {
		return nil, fmt.Errorf("extensions: mark disposed: %w", err)
	}
	forward, _ := json.Marshal(map[string]string{"runtime_handle": ext.RuntimeHandle})
	inverse, _ := json.Marshal(map[string]string{"action": "activate", "isolation": string(ext.Isolation)})
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO extension_effect (id, extension_id, extension_version, effect_type, forward_payload, inverse_payload, outcome)
		VALUES ($1, $2, $3, 'dispose', $4, $5, 'success')`,
		uuid.NewString(), id, version, string(forward), string(inverse),
	); err != nil {
		return nil, fmt.Errorf("extensions: record dispose effect: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("extensions: commit dispose: %w", err)
	}
	return r.Get(ctx, id, version)
}

// Get loads one extension row by id/version.
func (r *Registry) Get(ctx context.Context, id, version string) (*Extension, error) {
	var e Extension
	var description, statusReason, runtimeHandle sql.NullString
	var providesJSON, requiresJSON string
	err := r.DB.QueryRowContext(ctx, `
		SELECT id, version, name, publisher, description, entrypoint, isolation, provides, requires, status, status_reason, runtime_handle, manifest_digest
		FROM extension WHERE id = $1 AND version = $2`, id, version,
	).Scan(&e.ID, &e.Version, &e.Name, &e.Publisher, &description, &e.Entrypoint, &e.Isolation, &providesJSON, &requiresJSON, &e.Status, &statusReason, &runtimeHandle, &e.ManifestDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s@%s", ErrNotFound, id, version)
	}
	if err != nil {
		return nil, fmt.Errorf("extensions: get %s@%s: %w", id, version, err)
	}
	e.Description = description.String
	e.StatusReason = statusReason.String
	e.RuntimeHandle = runtimeHandle.String
	if err := json.Unmarshal([]byte(providesJSON), &e.Provides); err != nil {
		return nil, fmt.Errorf("extensions: parse provides: %w", err)
	}
	if err := json.Unmarshal([]byte(requiresJSON), &e.Requires); err != nil {
		return nil, fmt.Errorf("extensions: parse requires: %w", err)
	}
	return &e, nil
}

// List returns every discovered extension, newest first.
func (r *Registry) List(ctx context.Context) ([]*Extension, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT id, version FROM extension ORDER BY discovered_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("extensions: list: %w", err)
	}
	defer rows.Close()
	var out []*Extension
	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			return nil, fmt.Errorf("extensions: scan list row: %w", err)
		}
		ext, err := r.Get(ctx, id, version)
		if err != nil {
			return nil, err
		}
		out = append(out, ext)
	}
	return out, rows.Err()
}

// checkCompatibility enforces spec.compatibility.amhCore against this
// daemon's own CoreVersion — unconditional, unlike signature verification,
// since a manifest always declares this field (Validate already requires
// it non-empty) and there is no "not declared" case to leave unenforced.
func (r *Registry) checkCompatibility(m Manifest) error {
	ok, err := satisfiesRange(CoreVersion, m.Spec.Compatibility.AMHCore)
	if err != nil {
		return fmt.Errorf("%w: %q: %v", ErrIncompatibleCore, m.Spec.Compatibility.AMHCore, err)
	}
	if !ok {
		return fmt.Errorf("%w: this daemon is core %s, manifest requires %q", ErrIncompatibleCore, CoreVersion, m.Spec.Compatibility.AMHCore)
	}
	return nil
}

// checkSignature verifies m.Spec.Signature if present, and fails closed if
// absent only when r.RequireSignatures is set. See Discover's doc comment
// for the full admission rule.
func (r *Registry) checkSignature(ctx context.Context, m Manifest) error {
	sig := m.Spec.Signature
	if sig == nil {
		if r.RequireSignatures {
			return ErrSignatureRequired
		}
		return nil
	}
	if sig.Algorithm != "ed25519" {
		return fmt.Errorf("%w: unsupported algorithm %q", ErrInvalidSignature, sig.Algorithm)
	}
	digest, err := m.SignableDigest()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	if sig.Digest != digest {
		return fmt.Errorf("%w: signature.digest %s does not match the manifest's actual recomputed content digest %s", ErrInvalidSignature, sig.Digest, digest)
	}
	pub, err := r.Trust.verified(ctx, sig.KeyID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSignature, err)
	}
	sigBytes, err := hex.DecodeString(sig.Value)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return fmt.Errorf("%w: signature.value must be %d hex-encoded bytes", ErrInvalidSignature, ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, []byte(digest), sigBytes) {
		return fmt.Errorf("%w: ed25519 verification failed for key %s", ErrInvalidSignature, sig.KeyID)
	}
	return nil
}

func (r *Registry) requirementSatisfied(ctx context.Context, req Requirement) (bool, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT epc.capability_version
		FROM extension_provided_capability epc
		JOIN extension e ON e.id = epc.extension_id AND e.version = epc.extension_version
		WHERE epc.capability_id = $1 AND e.status = 'active'`, req.Capability)
	if err != nil {
		return false, fmt.Errorf("extensions: query providers of %s: %w", req.Capability, err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return false, err
		}
		ok, err := satisfiesRange(v, req.VersionRange)
		if err != nil {
			continue // an unparsable provided version can't satisfy a range; keep checking others
		}
		if ok {
			return true, nil
		}
	}
	return false, rows.Err()
}

type extRef struct{ id, version string }

func (r *Registry) activeDependents(ctx context.Context, capabilityID string) ([]extRef, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT id, version, requires FROM extension WHERE status = 'active'`)
	if err != nil {
		return nil, fmt.Errorf("extensions: query active extensions: %w", err)
	}
	defer rows.Close()
	var out []extRef
	for rows.Next() {
		var id, version, requiresJSON string
		if err := rows.Scan(&id, &version, &requiresJSON); err != nil {
			return nil, err
		}
		var reqs []Requirement
		if err := json.Unmarshal([]byte(requiresJSON), &reqs); err != nil {
			return nil, err
		}
		for _, req := range reqs {
			if req.Capability == capabilityID && !req.Optional {
				out = append(out, extRef{id, version})
				break
			}
		}
	}
	return out, rows.Err()
}

func (r *Registry) setStatus(ctx context.Context, id, version string, status Status, reason string) error {
	var reasonVal any
	if reason != "" {
		reasonVal = reason
	}
	_, err := r.DB.ExecContext(ctx, `UPDATE extension SET status = $1, status_reason = $2 WHERE id = $3 AND version = $4`, string(status), reasonVal, id, version)
	if err != nil {
		return fmt.Errorf("extensions: set status %s: %w", status, err)
	}
	return nil
}

func (r *Registry) recordEffect(ctx context.Context, id, version, effectType string, forward map[string]string, inverse map[string]string, outcome string) error {
	fj, err := json.Marshal(forward)
	if err != nil {
		return err
	}
	var ij any
	if inverse != nil {
		b, err := json.Marshal(inverse)
		if err != nil {
			return err
		}
		ij = string(b)
	}
	_, err = r.DB.ExecContext(ctx, `
		INSERT INTO extension_effect (id, extension_id, extension_version, effect_type, forward_payload, inverse_payload, outcome)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.NewString(), id, version, effectType, string(fj), ij, outcome,
	)
	return err
}
