# Existing-account login and automatic activation

This private deployment's owner authorized the backend to accept current iCloud,
GCBD and Apple media-service terms as part of account login and device activation.
The dashboard passes `-auto-activate` to its worker. Standalone workers retain the
explicit flag. This authorization does not cover purchases, subscriptions or
unrelated account repairs.

The worker first attempts GSA. On `repair`, it performs one iCloud web protocol
login, uses the dashboard for any SMS challenge, reads the actual terms version
when Apple identifies a terms update, and accepts that version. It then retries
GSA once. Other repair requirements stop; they are not treated as terms consent.
Apple can require separate web-login and GSA verification codes.

After verified GSA is saved, a failed iCloud delegate exchange can invoke the
account's own diagnostic Mac NAC runtime. The worker tries the v2 delegate with
NAC first. Only `MOBILEME_TERMS_OF_SERVICE_UPDATE` triggers reading and accepting
the current device-activation terms. The terms receipt must contain
`success=true` before the worker attaches `termsAccepted=true` and submits the
NAC exchange. MME issuance and a successful quota read are both required before
installing an account-bound checkpoint and resuming the online worker.

Private receipts are retained under `.cache/worker-activation-*`. Credentials,
codes, hardware and responses are not committed. Existing accepted checkpoints
are not overwritten. Unknown outcomes do not trigger automatic write retries.
The runtime uses this project's synthetic diagnostic Mac identity; it is not a
claim of physical Apple hardware. Terms acceptance does not establish push-code
delivery or authorize payment.

Validation includes offline web-repair transitions, code failures, exact-version
consent, terms acknowledgement, quota gating, and runtime isolation. A fresh
account still requiring web repair is needed for end-to-end live validation of
the newly connected automatic repair path; already activated accounts cannot
reproduce that state.
