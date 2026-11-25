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
- Startup no longer counts as a successful renewal. Maintenance checks the existing
  session, never performs periodic password authentication, and pauses after a
  failure. An explicit subsequent successful check resumes its schedule.
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
After deployment, verify protected workers remain absent, the unblocked worker PID
is unchanged, locked actions and bridge exports are refused, and old capabilities
are reported conservatively. Verify pushed branch SHA against the local commit.
