---
name: icloud-family-sharing
description: Inspect and manage Apple iCloud Family Sharing membership through the account's native protocol session. Use for family eligibility, member/invitation listing, inviting an account, accepting or rejecting an invitation, removing a member, leaving a family, or deleting a family. Do not use for CloudKit document sharing or App Store sandbox test families.
---

# iCloud Family Sharing

For the China organizer/member workflow (attended purchase, renewal cancellation, activation and joining), use [icloud-cn-family](../icloud-cn-family/SKILL.md). Its member branch never binds a payment method or purchases a separate plan.

Use the account's own GSA app tokens, accepted iCloud session, diagnostic Mac identity and Apple-returned URL bag. Start with read-only discovery; family membership writes require an exact current member or invitation binding and a post-action re-read.

For an eligible account with no family, creation and iCloud+ storage sharing are separate verified mutations. Create the empty family first with purchase sharing and location sharing disabled, re-read the family, then enable iCloud+ sharing only when the organizer has a live paid plan. Do not combine creation, storage sharing and invitation into one ambiguous request.

## Read-only discovery

Run from this repository:

```powershell
go run ./tools/familyinspect `
  -dir state/accounts/<account> `
  -gsa <fresh-private-gsa-checkpoint> `
  -dump <new-private-family-response>
```

The tool uses direct Apple HTTPS, keeps raw family data in an owner-only ignored file, and prints only counts, roles and eligibility. It does not submit a family mutation.

Require all of these bindings before any write:

- account directory `session.json` Apple ID and ADSID;
- the same account's `icloud-accepted.json` numeric iCloud DSID and live MME token;
- current `com.apple.gs.icloud.auth` and `com.apple.gs.icloud.family.auth` app tokens from the same GSA identity;
- the account's own native ADI and APS token;
- endpoints from the current `configurations/init?context=settings` response.

If family details returns Apple UI error but `checkFamilyEligibility` returns `eligible=true`, report that the account has no readable current family and is eligible to create or join one. Do not treat this as authentication failure or invent members.

Create a new organizer-only family with:

```powershell
go run ./tools/familyinspect -dir state/accounts/<account> -create-family
```

Completion requires a fresh family-details response with exactly one member where organizer=1 and self=1, with no invitations or pending members. This action does not enable App Store purchase sharing or location sharing.

After a paid iCloud+ plan is independently verified, enable only family storage sharing with:

```powershell
go run ./tools/familyinspect -dir state/accounts/<account> -share-icloud-storage
```

Completion requires current storage state `isParticipatingInFamilyStorage=true` and `isEnabled=true`, plus `ICLOUD_STORAGE` in the server's shared subscription group. Keep purchase sharing disabled unless the user separately requests and authorizes it.

For a local account pool, preflight every candidate without mutating it. Exclude the organizer, accounts without an accepted iCloud checkpoint, accounts already in a family, accounts with `eligible=false`, and accounts whose current Store offer currency/region is incompatible with the organizer. Do not invite every pool entry merely because it has a session directory.

When the target is locally available, prefer the verified in-person PET flow over fabricating a macOS Messages share result:

1. Keep the target worker online with its password only in process memory.
2. Start an ephemeral GrandSlam login for the target using the organizer's Mac ADI/APS identity and the target's current cookies.
3. If Apple requests trusted-device 2FA, submit one fresh code to that same ephemeral session. Never request SMS automatically.
4. Use the newly issued PET in `addFamilyMember` with `credentialSource=pet`, and keep purchase/location sharing defaults false.
5. Re-read both accounts and require two members, one organizer, and target `self=1`.
6. If the target reports `familyStorageAvailable=true` but `isParticipatingInFamilyStorage=false`, submit `JOIN_FAMILY` to `updateStorage`, then require participating, available and enabled all true.

The project worker action `family-add-self` performs the password handoff through a one-shot child-process environment and clears it immediately. The password must never enter command-line arguments, files, logs or API results. Apple may return HTTP 200 error documents; an HTTP status alone never proves acceptance.

For the current local dashboard workflow:

- `family-info` returns only the structured, redacted `FamilyInfo` summary. The dashboard may run this read-only helper from an accepted iCloud checkpoint even while the target worker is offline; do not poll Apple on every dashboard refresh.
- `family-add-self` takes the exact organizer account-directory ID in `code`. It performs the organizer PET mutation, then re-runs target-side membership discovery. If shared storage is available but the target is not participating, it executes the separate `JOIN_FAMILY` mutation and verifies it before returning.
- After Apple has created the ephemeral 2FA challenge, prefer a fresh code generated from the target account’s own bound local Mac/ADI identity. If local generation is unavailable, retain the same challenge and wait for `family-code.handoff`; never request SMS automatically.
- A timed-out `addFamilyMember` response is indeterminate, not a retry instruction. Perform a fresh organizer read-back for the exact target and then a target-side `self=1` read-back. Treat the operation as successful only when current Apple state confirms membership; otherwise stop without replaying the mutation.
- Completion requires current membership plus `storage_known=true`, `storage_participating=true`, `storage_available=true`, `storage_enabled=true`, and the expected shared capacity. A transport response, invitation, or `familyStorageAvailable=true` alone is incomplete.

The WebUI family card must expose the same state machine: select a registered organizer, show current counts/role/storage, keep purchase and location sharing off, and display partial membership separately from shared-storage completion. It must consume structured `FamilyInfo`, never scrape helper logs or expose private family identifiers.

The dashboard persists the redacted result per account in `family-status.json` with an account-directory binding and `checked_at`. This cache exists only to restore the UI after refresh or dashboard restart. Show cached state with its verification time; retain but mark it stale after the configured freshness window. A stale cache must disable join/storage mutations until a fresh Apple read succeeds. Never treat cached eligibility, membership, organizer identity, pricing or storage availability as write authorization. Passwords, PETs, tokens, cookies, verification codes, family IDs and raw responses are never cached.

## Membership mutations

Read [references/protocol.md](references/protocol.md) before implementing or submitting add, remove, accept, reject, leave or delete operations.

For removal, bind the target to the latest `family-members` response and require the acting account to be the organizer. Refuse self, organizer and child-account removal unless Apple explicitly returns a supported action for that exact member. Submit the Apple-returned numeric `member-dsid` once, then re-fetch family details and require the member to be absent.

For invitations, validate the recipient identifier, use the current `addFamilyMember` endpoint and preserve the server invitation code/URL. Completion requires the recipient to accept and a fresh family-details response showing the member. An invitation response alone is incomplete.

Leaving or deleting a family affects purchase sharing, subscriptions, child accounts and shared services. Bind the action to the current account's role and current family ID, then obtain explicit authorization for the concrete family and action immediately before submission.

Never log Apple IDs, names, phone numbers, member DSIDs, family IDs, invite codes, tokens, cookies or raw responses. Never reuse a member or invite identifier from another account or an older family snapshot.
