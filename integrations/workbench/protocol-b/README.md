# Protocol B workbench compatibility

`workbench.patch` restores the workbench integration on top of sentinel-rs
`ef04faf2e120acc62e5c2d558593b7cdf2831656` (Chrome 150–154). It does not change
`fingerprint.rs`, Cargo dependencies, or the 2/3/5/10/80 version weights.

The patch keeps local mailbox requests off the registration proxy and exports
the registration device into a separate SQLite `registration_devices` table.
External requests retain the configured proxy. Device metadata is optional;
an archive error cannot turn an already registered account into a failed signup.
The original device record is preserved on repeated writes.

Copy `build.ps1` and `workbench.patch` into the workbench's `protocol_b` directory.
Run `build.ps1 -SourceDir <sentinel-rs checkout>` with the existing toolchain.
The script applies the patch only when necessary, rejects conflicts, clears
only this crate's release cache, and runs the fingerprint, local proxy routing,
device archive, flow, and Sentinel integration tests before deploying the EXE
and its SHA256. A missing test/patch or a failing test blocks deployment.

The Windows build paths remain configurable through `-ToolRoot`. No account
data, credentials, binaries, logs, or runtime databases belong in this directory.

Device export is evidence preservation, not a guarantee that the Python
maintenance transport can restore every B browser profile. Historical runs
without a structured export still use the existing partial log fallback.
