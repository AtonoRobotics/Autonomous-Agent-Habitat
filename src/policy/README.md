# policy

The deterministic policy gateway every effect request passes before anything else.

Rule bodies come from the pack; enforcement is core. Any matching deny wins; no
matching allow fails closed. Core refusals (mail-claimed authority, assumed
autonomy, prohibited ceilings) fire before pack rules are consulted.
