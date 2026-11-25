# Device service protection and health reporting

## Account protection

`account-guard.json` in an account's own directory is independent of runtime,
session and family caches. A blocked, unreadable, malformed or unknown-version
record prevents worker startup, background checks, control actions, activation,
family helpers and mail-bridge credential export. No login or cache refresh clears
it. Absence of a record is compatible with existing installations.

When protecting a worker from an older release, first persist the guard, request
a graceful stop, and preserve `failure_reason=verification_blocked` in its stopped
runtime record. This also inhibits the older dashboard's automatic bootstrap.
Do not delete its session, device, accepted checkpoint or family information.

After the owner confirms recovery through an official trusted flow, the explicit
local release command is:

```powershell
go run ./tools/accountguard -dir <exact-account-directory> -release -confirm-recovered
```

It retains the protection record and release time, and starts no process or login.
Legacy verification warnings remain until an explicit subsequent login. Never use
repeated login or OTP requests to discover whether a locked account has recovered.

## Health meanings

- Process ownership and APS connectivity are independent of session validity.
- A successful device query within 15 minutes, with a healthy current state, is
  reported as a recent device-service verification. This does not verify every
  Apple service, device trust, or OTP acceptance.
- Startup alone is not success. Only a successful device query followed by a saved
  checkpoint counts as a successful existing-session check; this does not prove
  token expiry was extended. Maintenance never performs password authentication.
  Temporary transport failures and plain HTTP 5xx device-query responses retry
  after 1, 5 and 15 minutes, then pause. Explicit service error codes, 401/403/429,
  identity changes, unknown errors and failed saves pause immediately. The retry
  budget, deadline and pause survive restart. An interrupted in-flight check pauses
  rather than silently replaying. Successful explicit checks resume the schedule.
- Resume mode ignores injected passwords, skips OTP preflight and device
  registration/activation, and preserves the account push-token binding. Failed
  device checks never overwrite the saved session. Request cancellation reaches
  the device-list HTTP transport; diagnostic errors contain no raw transport text.
- Fixed diagnostic stages distinguish local session reuse, service-session access,
  device query and saving. Errors expose only an allowlisted category and numeric
  HTTP status, never arbitrary upstream text, URLs, tokens or response bodies.
- Bridge readiness requires a live worker advertising the new protocol and
  available credentials, a recent device-service check, fresh family sharing and
  fresh capacity data. Historical completion receipts do not override stale data.
  Old workers remain observable but cannot advertise readiness for unsupported
  bridging or request-approval capabilities. A 404 identifies a version mismatch.
- Approval requests expire after five minutes. Both the dashboard and new worker
  enforce expiry; the execution path checks again after queueing. Old workers'
  approval actions are unavailable through the new dashboard.

## Windows permissions and deployment

The runtime principal, SYSTEM and Administrators retain access. New account
directories and temporary session/control files inherit an explicit private DACL.
An existing-installation migration must first capture original DACLs in a private,
ignored backup directory, then apply only to the intended account directories;
reject reparse points and verify the process owner's identity before changing ACLs.
Only DACLs need to change; owner and audit ACL privileges are unnecessary.

Build and test new dashboard/worker binaries before replacement. Retain original
binaries, their hashes, launch arguments and DACL backup outside Git. Replacing a
disk executable does not upgrade its already-running process. The dashboard can be
restarted independently while an existing unblocked worker remains running.
Protected workers must remain stopped. A running old worker must continue to be
reported as lacking new capabilities until a separately planned restart.

Rollback must preserve account protection and stopped-worker compatibility markers.
Never launch an old worker for a protected account or restore runtime snapshots over
current session material. ACL rollback, if necessary, should restore the saved DACL
for each exact path without traversing other directories. Runtime backups, ACL
inventories, account state, logs, binaries and credentials must never enter Git.

## Verification

Run the accountguard, privatefs, control, dashboard and trusted-device Go tests,
including `-race`, plus `node --check internal/dashboard/web/app.js` and
`git diff --check`. Test UI states with fixture data, without login, OTP or commerce.
After deployment, verify protected workers remain absent, only the authorized worker PID
changes, locked actions and bridge exports are refused, and old capabilities
are reported conservatively. Verify pushed branch SHA against the local commit.

## Controlled existing-session diagnostic, 2026-11-25

The authorized organizer worker was restarted using the same saved identity and
session, with password injection and automatic activation disabled. Resume startup
skipped OTP generation and registration. Its single device-service check completed
local restoration and local service-client availability, then received HTTP 401 at
the device-query stage. Saving was not reached. Automatic checks are paused with
no retry deadline; the APS connection remains connected independently.

Hashes of the saved session, device identity, push state and accepted iCloud
checkpoint were unchanged. The three guarded workers stayed stopped, including
across the dashboard update. The new worker advertises the bridge protocol but has
no available bridge credentials; bridge readiness is false, with the unverified
device session already sufficient to block export. Family and quota caches were
not refreshed after the authentication refusal and remain unverified.

This confirms that the existing device-management request is rejected, not why:
HTTP 401 alone does not establish token expiry or account lock. Local restoration
does not renew a server token, and APS connectivity does not validate the device
management session. Obtaining newly accepted device-management credentials would
require a separate, owner-involved authentication flow; no password login, code
generation or repeated diagnostic request was performed during this deployment.

Private rollback material retains the old panel and launch metadata, the verified
new worker and hashes, and the four unchanged credential/identity files. Panel
rollback must keep the protected new worker and current pause record. Never restore
an older worker that lacks persistent check suppression, or overwrite a live
session with a backup merely because the service returned 401.
