# Windows activation runbook

Use the repository's background CLI from its root. This runbook requires an account already created through the project, with its own `device.json` and account-bound `session.json`. A saved session is a binding record; its presence does not prove current GSA or iCloud authentication. Create a missing account through the project's existing account setup path rather than copying another account's state.

## 1. Prepare private state and direct transport

Select the requested account and a new run directory. The examples use placeholders, never a real account identifier or secret. Keep credentials in process environment supplied by the owner or project input. Do not put them in command arguments or committed scripts.

```powershell
$accountDir = 'state/accounts/<selected-account>'
$runDir = Join-Path '.cache' ('activation-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $runDir -ErrorAction Stop | Out-Null
$principal = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
icacls $runDir /inheritance:r /grant:r "${principal}:(OI)(CI)F" | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Private directory ACL failed' }
git check-ignore $runDir
if ($LASTEXITCODE -ne 0) { throw 'Private directory must be Git-ignored' }

# Apply when the owner requested direct Apple connections.
@('HTTP_PROXY','HTTPS_PROXY','ALL_PROXY','APPLESERVICES_SOCKS_PROXY') |
  ForEach-Object { Remove-Item "Env:$_" -ErrorAction SilentlyContinue }
$env:NO_PROXY = '*'
$env:APPLESERVICES_DELEGATE_PATH = 'v2'

go build -o bin/accountsetupcheck.exe ./tools/accountsetupcheck
go build -o bin/nacexchange.exe ./tools/nacexchange
go build -o bin/macpushcheck.exe ./tools/macpushcheck
```

Check each build exit code before running it. Go's file mode `0600` is insufficient to establish owner-only Windows permissions by itself; use the restricted parent directory. Back up existing accepted state privately before any installation. Never rotate an accepted account identity just to retry a failed login.

## 2. Independent Mac APS and native ADI

Read [mac-isolation.md](mac-isolation.md) before repairing an existing profile. Each account needs its own UUID, serial, synthetic MAC/effective ROM/MLB, ADI state and Apple-issued APS keypair/token. Compare equality booleans, never print identifiers. Synthetic fields describe a diagnostic Mac, not user-owned official hardware.

The currently verified native module is `.cache/openbubbles-store/rust_lib_bluebubbles.dll`, SHA-256 `ca88e816dda449d7ba24d54a5ed709a2d392daeae28c25a61dcd2e7496e44cfe`. Verify it before loading. This is the Mac-style ADI/APS bridge; installed Windows CoreADI is a separate diagnostic implementation.

For a new diagnostic profile, or an explicitly selected private repair copy:

```powershell
$profileDir = Join-Path $runDir 'native-adi'
python tools/mac-push-check.py --profile $profileDir `
  --device "$accountDir/device.json" `
  --library .cache/openbubbles-store/rust_lib_bluebubbles.dll `
  --go-state-out "$runDir/push.json"
.\bin\macpushcheck.exe -dir $accountDir -profile $profileDir `
  -state "$runDir/push.json"
.\bin\accountsetupcheck.exe -dir $accountDir `
  -native-profile $profileDir -native-only
```

These calls provision APS/ADI but do not authenticate an account or activate iCloud. Only after profile and push binding verification, stop the affected project worker and install the matching profile and `push.json` with private rollback copies. Existing accepted accounts use their current profile and push state. Subsequent commands below use the installed `$accountDir/native-adi`.

## 3. Generate account-bound NAC

Prepare an account-private runtime containing the pinned pypush `emulated` hooks and dependencies, an `emulated/IMDAppleServices` matching the upstream `BINARY_HASH`, and the validated XNU property-encryption DLL. Read [open-source.md](open-source.md) if these are missing. This checkout stages source under `.cache/activation-source-review` and a runtime under `.cache/mac-nac-runtime`; inspect their revisions and contents before reusing code. Copy code and dependencies only, replacing donor/generated `data.plist` with this account's data through the tool. Do not copy another account's certificates, validation, hardware or sessions.

```powershell
$nacRuntime = Join-Path $runDir 'nac-runtime'
# Stage the inspected runtime code/dependencies into $nacRuntime first.
python tools/mac-nac-check.py `
  --profile "$accountDir/native-adi/hw_info.plist" `
  --runtime $nacRuntime `
  --encrypt-dll .cache/activation-source-review/mac-io-encrypt.dll `
  --validation-out "$runDir/nac-validation.bin" `
  --synthetic-missing-board-fields --transport bin/nacexchange.exe
```

Use the synthetic flag only for an explicitly selected diagnostic profile; omit it for complete owner-supplied hardware. Require successful init, Apple session-info exchange, key establishment and signing on one context. Previously accepted runs produced 517 bytes, but length alone is not acceptance. Preserve hardware/validation binding privately. NAC output does not prove activation.

## 4. Complete fresh GSA in one live challenge

For an owner who uses existing trusted devices:

```powershell
.\bin\accountsetupcheck.exe -dir $accountDir -trusted-device-code `
  -sms-code-file "$runDir/device-code.txt" `
  -checkpoint "$runDir/gsa.json" -dump "$runDir/setup.html"
```

The misleadingly named `-sms-code-file` is also used by device mode. The file must not exist when the challenge starts. Wait for `trusted-device code request accepted` and `awaiting a code for this live challenge`; then obtain a new owner-provided code and write it privately to that path while the CLI is still running. Do not start a replacement login, combine SMS flags with device mode, or reuse a code from history. Delete the code file after use. The CLI's context expires after three minutes; prepare builds and paths before requesting a code.

For an explicitly selected local-code diagnostic, keep that live challenge open and hand the bound Mac's code directly to its new private file:

```powershell
go build -o bin/localcodehandoff.exe ./tools/localcodehandoff
.\bin\localcodehandoff.exe -dir $accountDir -out "$runDir/device-code.txt"
```

The helper never prints the code and refuses to overwrite a file. Delete the handoff after the live process consumes it. A six-digit output is only local generation evidence; require Apple to accept it. If validation returns `-21669`, stop local-code attempts for that account and use an actually available trusted device or phone recovery path.

For explicitly chosen SMS delivery, omit `-trusted-device-code` and use the same private-file handoff. If Apple reports `tooManyCodesSent=true` / `-22981`, stop requesting SMS. Do not infer recovery duration. If no 2FA is required, the same command completes directly. Local generation is a separate opt-in diagnostic and requires demonstrated acceptance for the same account; device trust alone is insufficient.

When the owner has only this account's local diagnostic Mac, test delivery without approving a notification or completing the requester login:

```powershell
.\bin\trustedcodecheck.exe -dir $accountDir -work "$runDir/push-observation" `
  -listen-for 30s -push-only -saved-receiver-binding
```

This mode uses the saved account binding only to attribute APS events; it does not establish current GSA trust. Report APS connection, matching token, received sign-in notices, and server acceptance of any separately tested local code as independent facts. A connected APS channel with zero notices does not make the Mac a working trusted device. A six-digit local output rejected by Apple also fails the 2FA acceptance requirement; stop instead of repeatedly generating codes.

Keep the completed `$runDir/gsa.json`. Code verification can return a usable PET directly; immediately resume this checkpoint for terms and delegate. A later password login may invalidate it or change the result.

## 5. Terms, then v2 delegate

Read the returned setup page privately and summarize region and exact iCloud/media version IDs. Terms acceptance requires the owner's authorization for those terms; existing matching authorization in the conversation is sufficient. Do not hard-code another account's region or versions.

Apple can return an iCloud-only update where `tosversion_field` is numeric and `amstosversion_field` is present but empty. Preserve that empty media version and present the approval tuple as `<region>:<icloud-version>:`. Do not invent or copy a media-service version from another account. After explicit approval, submit `ams-tos-version` as empty and still require the server's explicit `success=true` acknowledgement.

```powershell
.\bin\accountsetupcheck.exe -dir $accountDir -resume "$runDir/gsa.json" `
  -accept-terms -expected-terms '<region>:<icloud-version>:<media-version>' `
  -dump "$runDir/terms.html"
```

Require explicit `success=true`. HTTP 200 alone is insufficient. Preserve the acknowledgement and approved version tuple privately. If there was no terms update, omit this call and the `-terms-accepted` switch below. If a terms version changes, the old approval does not cover it.

```powershell
.\bin\accountsetupcheck.exe -dir $accountDir -resume "$runDir/gsa.json" `
  -delegate-only -terms-accepted `
  -nac-validation "$runDir/nac-validation.bin" `
  -dump "$runDir/delegate.plist" `
  -icloud-checkpoint "$runDir/icloud-accepted.json"
```

The tool sends v2 camelCase outer keys, keeps nested `client-id`, attaches NAC and uses the bound native headers. It writes the accepted checkpoint only after delegate and quota success. Require `mme-token-present=true` and `quota accepted`; parse status details using [acceptance.md](acceptance.md). Save an accepted checkpoint to the account directory only after those checks, retaining private rollback state and correct ACLs.

## 6. Read-only verification and limitations

```powershell
.\bin\accountsetupcheck.exe -dir $accountDir `
  -icloud-resume "$runDir/icloud-accepted.json" -storage-plan

.\bin\accountsetupcheck.exe -dir $accountDir -resume "$runDir/gsa.json" `
  -devices-only -dump "$runDir/devices.json"
```

The first command reads quota/offers with the accepted MME and does not log in or buy. The second inspects device trust separately; an expired GSA can fail while the accepted MME still reads quota. Report `trustedDevice`, serial-match boolean, `supportsVerificationCodes` and `pushDevice` independently. Actual code acceptance and push delivery require separate tests.

Verified standalone activation does not fix the worker automatically. Compare fresh-session source, identity, serial override, terms cookie, registration ordering and headers when the worker returns reason 3001/3005. Do not replace a working MME merely because a later login fails. Activation and offers do not establish Store authentication or payment. For purchasing, use `.claude/skills/icloud-plus-purchase/SKILL.md`, with its physical Store GUID and separate success verification.


## Connection recovery before authentication

The normal worker now recovers bounded connection-setup failures without a temporary proxy or a pinned Apple IP:

- GSA first uses the configured transport. Only an EOF/network failure before HTTP receives a connected socket can trigger recovery. It queries `https://dns.google/resolve?name=gsa.apple.com&type=A` (six-second limit) and tries at most two public IPv4 results within the request's existing deadline. This query contains only the public hostname, never account data.
- Recovery preserves the original proxy/NO_PROXY decision, HTTP Host, TLS SNI and certificate verification. It does not retry certificate rejection, HTTP status errors, or a disconnect after a connection was handed to HTTP, because authentication may already have been submitted. A request body without a replay factory also stops.
- APS retries only TCP/TLS establishment, at most three attempts with a ten-second limit per attempt and the caller's deadline. Device authentication starts once after TLS succeeds. Certificate rejection stops immediately.

Run `go test ./gsa ./aps ./cmd/trusted-device ./internal/appleweb` for the regression checks. The credential-free live check is opt-in: set `APPLESERVICES_TEST_GSA_NETWORK=1`, then run `go test ./gsa -run '^TestGSAConnectionRecoveryLive$' -v -count=1`. It checks a normal request and an injected first-handshake EOF against GSA, with verified TLS and the expected GET 404. It never sends credentials or requests a verification code. Clear the test variable afterward.

Successful connection recovery is not activation evidence. Still verify the account's online state and a fresh read-only quota response. A persistent outage, unavailable recovery DNS, or post-submission failure remains an error; do not promise recovery from every network failure or replay SMS automatically.
