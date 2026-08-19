# auth

Principals, tokens, and authorization cards.

Two principal kinds: `field` and `architect`. Every configuring verb is behind an
Architect check; a field token that reaches one gets `SURFACE_VIOLATION`.

One card per external effect, in the pack's field language. Deny is terminal — the
same agent + action class + subject + channel is not silently resubmitted.
