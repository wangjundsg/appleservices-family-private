# Repository workflow

- After completing and verifying a user-requested code, documentation, WebUI, backend, or repository-managed skill change, commit it and push it to the configured private remote unless the user explicitly says not to for that change.
- Stage only files owned by the current task. Preserve unrelated modified and untracked files.
- Run tests appropriate to the changed area and `git diff --check` before committing. After pushing, verify that the remote branch resolves to the local commit.
- Never commit account state, passwords, OTPs, cookies, tokens, DSIDs, PETs, authenticated responses, logs, generated binaries, or other sensitive/runtime artifacts.

# Activation recovery

- For an already registered account whose first iCloud activation failed, the owner's recovery policy is to restart the activation flow with fresh password authentication (`APPLESERVICES_FRESH_GSA=1`), completing any verification Apple requests, then NAC, activation, device registration, and online confirmation. Do not start recovery with `-activate-saved` or a pipeline resume that reuses the failed GSA session.
- Keep the existing account, device identity, and purchased resources; do not interpret “start activation from the beginning” as registering another account. Preserve private failure evidence before a new attempt. See `docs/REUSE-GUIDE.md`, section nine, for the distinction between fresh recovery and passing newly issued state within that attempt.
- Local activation uses direct IPv4 scoped to its subprocesses, including native ADI/APS DNS; do not disable system IPv6 or change browser/Clash routing. Confirmed recoverable mid-activation failures may restart the full fresh-authentication flow once per task within existing SMS limits. Never replay an ambiguous write or bypass Apple rate limits; preserve the exact failure category after the bounded restart.
