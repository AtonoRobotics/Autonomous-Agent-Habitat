// Package extensions is the real implementation of the Cordis-lifecycle
// CapabilityRegistry the v10 specification calls for (§1 decision 6:
// "Cordis spatiotemporal composition governs extension lifecycle") and
// contracts/extension-manifest.schema.json defines the wire shape of.
// store/migrations/0001's capability/capability_effect tables were this
// concept's DDL only, with no code ever reading or writing them — this
// package, and 0002_control_plane.sql's extension/extension_effect tables,
// replace that gap with a working registry: discover, activate (spatial
// composability — dependency resolution and activation order), quiesce,
// dispose (temporal composability — disposal as activation's verified
// inverse), and rollback on partial-activation failure.
package extensions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
)

// Isolation mirrors extension-manifest.schema.json's spec.isolation enum.
type Isolation string

const (
	IsolationProcess   Isolation = "process"
	IsolationWasm      Isolation = "wasm"
	IsolationContainer Isolation = "container"
	IsolationInProcess Isolation = "in_process"
)

// CapabilityRef mirrors the manifest's $defs/capability — one thing an
// extension provides.
type CapabilityRef struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Contract string `json:"contract,omitempty"`
}

// Requirement mirrors $defs/requirement — one capability an extension
// depends on to activate.
type Requirement struct {
	Capability   string `json:"capability"`
	VersionRange string `json:"versionRange"`
	Optional     bool   `json:"optional"`
}

// ActionType mirrors $defs/actionType.
type ActionType struct {
	ID             string `json:"id"`
	SchemaVersion  string `json:"schemaVersion"`
	InputSchema    string `json:"inputSchema"`
	OutputSchema   string `json:"outputSchema"`
	Reconciliation string `json:"reconciliation"`
}

// Compatibility mirrors spec.compatibility.
type Compatibility struct {
	AMHCore   string   `json:"amhCore"`
	Platforms []string `json:"platforms,omitempty"`
}

// Signature mirrors spec.signature.
type Signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"keyId"`
	Digest    string `json:"digest"`
	Value     string `json:"value"`
}

// Metadata mirrors the manifest's top-level metadata object.
type Metadata struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Publisher   string `json:"publisher"`
	Description string `json:"description,omitempty"`
}

// Spec mirrors the manifest's top-level spec object.
type Spec struct {
	Entrypoint    string          `json:"entrypoint"`
	Isolation     Isolation       `json:"isolation"`
	Provides      []CapabilityRef `json:"provides"`
	Requires      []Requirement   `json:"requires"`
	Actions       []ActionType    `json:"actions,omitempty"`
	Schemas       []string        `json:"schemas,omitempty"`
	Migrations    []string        `json:"migrations,omitempty"`
	Compatibility Compatibility   `json:"compatibility"`
	Signature     *Signature      `json:"signature,omitempty"`
}

// Manifest mirrors contracts/extension-manifest.schema.json in full — an
// extension declares itself with exactly this shape, whether it's a
// harness, a connector, a knowledge-base implementation, a model
// provider, or a user-surface extension like the control-plane UI.
type Manifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

var (
	namespacedIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*/[a-z0-9][a-z0-9._-]*$`)
	semverPattern       = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
)

// Validate checks the manifest against the contract's structural
// requirements this package actually enforces (full JSON Schema validation
// is deliberately not reimplemented here; this covers what Activate and
// dependency resolution rely on being true).
func (m Manifest) Validate() error {
	if m.APIVersion != "amh/v1" {
		return fmt.Errorf("extensions: apiVersion must be \"amh/v1\", got %q", m.APIVersion)
	}
	if m.Kind != "Extension" {
		return fmt.Errorf("extensions: kind must be \"Extension\", got %q", m.Kind)
	}
	if !namespacedIDPattern.MatchString(m.Metadata.ID) {
		return fmt.Errorf("extensions: metadata.id %q is not a valid namespaced id", m.Metadata.ID)
	}
	if m.Metadata.Name == "" {
		return fmt.Errorf("extensions: metadata.name is required")
	}
	if !semverPattern.MatchString(m.Metadata.Version) {
		return fmt.Errorf("extensions: metadata.version %q is not valid semver", m.Metadata.Version)
	}
	if m.Metadata.Publisher == "" {
		return fmt.Errorf("extensions: metadata.publisher is required")
	}
	if m.Spec.Entrypoint == "" {
		return fmt.Errorf("extensions: spec.entrypoint is required")
	}
	switch m.Spec.Isolation {
	case IsolationProcess, IsolationWasm, IsolationContainer, IsolationInProcess:
	default:
		return fmt.Errorf("extensions: spec.isolation %q is not one of process|wasm|container|in_process", m.Spec.Isolation)
	}
	if m.Spec.Compatibility.AMHCore == "" {
		return fmt.Errorf("extensions: spec.compatibility.amhCore is required")
	}
	for _, p := range m.Spec.Provides {
		if !namespacedIDPattern.MatchString(p.ID) {
			return fmt.Errorf("extensions: provides capability id %q is not a valid namespaced id", p.ID)
		}
		if !semverPattern.MatchString(p.Version) {
			return fmt.Errorf("extensions: provides capability %q version %q is not valid semver", p.ID, p.Version)
		}
	}
	for _, r := range m.Spec.Requires {
		if !namespacedIDPattern.MatchString(r.Capability) {
			return fmt.Errorf("extensions: requires capability id %q is not a valid namespaced id", r.Capability)
		}
		if r.VersionRange == "" {
			return fmt.Errorf("extensions: requires capability %q missing versionRange", r.Capability)
		}
	}
	return nil
}

// Digest returns the manifest's content digest ("sha256:<hex>"), matching
// the format extension-manifest.schema.json's $defs/sha256 expects for
// signature.digest — recorded on the extension row so a later re-Discover
// of the same id/version can detect the manifest changed underneath it.
func (m Manifest) Digest() (string, error) {
	canonical, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("extensions: marshal manifest for digest: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// SignableDigest is the content digest a manifest's spec.signature attests
// to: the same construction as Digest, but computed with the signature
// field itself stripped first, since a signature cannot attest to its own
// bytes. The registry recomputes this server-side and requires it to equal
// the manifest's declared signature.digest exactly — the same "never trust
// a caller-supplied digest" property daemon/policy enforces for action
// digests.
func (m Manifest) SignableDigest() (string, error) {
	m.Spec.Signature = nil
	return m.Digest()
}
