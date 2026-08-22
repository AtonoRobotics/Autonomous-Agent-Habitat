# AMH Core Contracts

These JSON Schemas are the stable domain-neutral surface between AMH and extensions.

| Contract | Purpose |
|---|---|
| `extension-manifest.schema.json` | Declares extension identity, compatibility, capabilities, dependencies, action types, schemas, and isolation. |
| `action-envelope.schema.json` | Carries one extension-owned action proposal and its declared properties into durable admission. |
| `effect-record.schema.json` | Records the generic lifecycle and uncertainty of a core-mediated effect. |
| `policy-decision.schema.json` | Binds a policy result to the exact action digest and expiry. |

The schemas deliberately do not define devices, physical locations, safe states, financial instruments, customers, properties, or any other domain entity. Domain extensions publish namespaced schemas and reference them from their extension manifest.

Reversibility is represented as a generic declared property. The owning extension defines, verifies, and invalidates the attestation and owns recovery semantics. The AMH core may use the property as a policy predicate but does not derive or interpret the inverse.

All contracts use JSON Schema 2020-12. CI must validate the schemas themselves, validate canonical fixtures, reject additional undeclared core fields, and enforce semantic-version compatibility before an extension can activate.
