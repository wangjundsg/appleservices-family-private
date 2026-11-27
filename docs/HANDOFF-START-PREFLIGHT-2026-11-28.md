# Local Chrome registration: activation start preflight

The observed registration completed successfully, but activation `start` failed
before launching the account worker. The account view retained
`native-route-tls`; its account directory contained only the provisioned device
and registration origin. No login worker logs or authenticated state existed.

The configured native HTTP proxy repeatedly failed the TLS handshake. Using the
existing direct routing configuration passed the credential-free native, GSA,
verification, NAC and APS preflights. Deployment now persists that direct route
in its private network configuration. Certificate verification remains enabled;
no automatic proxy rotation or TLS bypass was added. A successful preflight is
transport evidence, not proof of completed account activation.

The handoff adapter previously discarded the preflight stage and left an
ambiguous `intent` receipt. It now persists an allowlisted pre-login failure as
`not_started`, with its stage and `panel_start_preflight_failed` reason. An
explicit resume can retry this same start intent. Unknown outcomes, changed
intents and other mutations remain blocked from replay. The Python pipeline
retains the failure and resumes at login for an already-created account.

Validation: dashboard Go tests passed; 185 pipeline tests passed. Regression
tests cover durable preflight failures, safe retries and resuming without
registering again. The deployed panel uses manual worker startup; the three
previously running account workers retained their process IDs and start times.

After explicit execution approval, the original task resumed successfully:
`start` and SMS verification succeeded, with an account-bound GSA session saved.

## Activation ownership

The next failure exposed competing activation flows. The worker starts its
existing automatic activation after GSA verification, but the pipeline treated
the GSA checkpoint as a reason to stop it and run the dashboard's separate
terms-first flow. The observed stop was followed by a worker NAC failure; the
separate terms query returned HTTP 401.

The pipeline now waits for the worker's accepted iCloud checkpoint instead of
stopping it at GSA. Explicit recovery of a stopped, verified account invokes
the same worker activation function with `-activate-saved`; it does not enter
password login, request a code, or register again. The account-bound GSA/PET
checkpoint is required. Existing accepted accounts are not activated again.
The new handoff action retains intent receipts to prevent replay of unknown
activation outcomes. Cancellation is distinguished from NAC failure, with
failed NAC component output retained only in the private runtime directory.

Validation: trusted-device and dashboard Go tests passed; 186 pipeline tests
passed outside the restricted sandbox. Coverage includes waiting through GSA
without stop/plan calls, saved activation with expired SMS, and rejection of
missing or mismatched GSA credentials.

Live recovery completed NAC generation, then Apple's delegate exchange returned
`ErrorID=UNAUTHORIZED` with the description that the account credentials were
unauthorized. No accepted iCloud checkpoint was created. This is distinct from
the resolved TLS preflight failure and interrupted NAC generation. The response
does not establish whether the saved credential expired or was rejected for
another reason. No endpoint switching, repeated delegate submission, new
registration, or changes to existing activated workers were performed.

The failed activation keeps its intent receipt to prevent replay. Subsequent
failures retain the allowlisted activation stage in the pipeline result, and
the saved-session activation process honors the account's stop request. A new
regression verifies that failure details survive without permitting replay.

## Owner-requested fresh login

The owner subsequently requested fresh password/SMS login for the same account.
The rejected session and prior action receipts were archived privately, retaining
the same task, device identity and phone order. One code reservation consumed by
the proven pre-login TLS failure was released with an audit record. Used-code
history was retained. No new registration or number purchase occurred.

The fresh login completed without an additional SMS challenge. The original
worker completed NAC and activation, saved the accepted iCloud checkpoint, and
became authenticated and online. Readback completed the original task with
`ready` and `handoff_complete=true`. The three previously activated workers
retained their original process IDs and start times.

The initial task observer briefly read the previous worker's failed runtime
status before the new worker published its own status. The new worker continued
successfully; completion was reconciled through the existing readback flow.
This startup observation race remains a separate follow-up.

The reconnect form also cleared the password before knowing whether the server
accepted the request. It now retains the input on rejection or transport error,
and clears it after success, account creation/selection, or dialog closure.
The UI regression covers both rejected and failed requests plus successful
submission; passwords remain only in the open form and are not persisted.

## Transient connection failure during English acceptance

The next successful English registration stopped at `native-route-connect`.
A credential-free direct request reproduced a TCP connection timeout before
TLS. The configured local proxy separately failed its TLS handshake and was
not enabled. The unchanged direct route subsequently passed every preflight.
Subsequent DNS/dial tracing showed two different addresses for the same public
hostname: one consistently timed out before TLS, while the other passed the
repository's certificate checks and returned HTTP 404. System DNS later returned
the reachable address. No static IP override, proxy change or certificate bypass
was applied; the reason the upstream DNS/routing supplied the unreachable address
remains unconfirmed.

HTTP preflight now retries once only when no connection has been acquired and
the error is a connection/DNS timeout or transport interruption. Certificate
failures and failures after acquiring a connection are not retried. Diagnostics
retain an allowlisted failure category and attempt count in the pre-login
receipt. Login, SMS and activation requests are not replayed by this retry.
Dashboard tests cover recovery, bounded failure, certificate rejection and
response failures; the full dashboard package passed.

The first live retry still timed out twice. Both failures remained `not_started`,
and the account directory contained only its initial identity/origin files.
After the DNS result recovered and every preflight passed, one unused login/SMS
reservation was released with private audit evidence. The original task then
started its worker and reached SMS verification without registering again.

The original English task then completed automatic SMS verification and the
worker-owned activation. Independent panel readback confirmed activated,
authenticated and online; the pipeline recorded `ready` with
`handoff_complete=true`. All four previously activated workers retained their
original process IDs and start times. The bounded retry does not repair an
upstream route that remains unavailable across both attempts.

## Actual transport correction and fixed direct activation

The owner requested investigation beyond public preflight responses. The audit
found three application defects distinct from the observed unreachable DNS
address: automatic activation inherited proxy overrides while other activation
paths removed them; the GSA connection timeout could consume the entire
30-second request deadline before recovery; and recovery depended exclusively
on Google DoH, which was unreachable on this direct connection.

Local activation now uses one shared direct child environment at worker launch,
saved/manual activation, worker-owned NAC/delegate execution and Mac APS
bootstrap. It removes all HTTP/HTTPS/ALL/native/SOCKS proxy overrides, including
mixed-case names and inherited NO_PROXY values. The local launcher accepts only
the existing all-empty proxy configuration with NO_PROXY=*. Provider acquisition
and browser creation remain outside this child-environment transformation.

Public HEAD checks no longer gate worker startup or second-factor delivery.
Startup still validates local libraries/runtime; actual native initialization,
authentication and activation determine success. The optional diagnostic CLI
is retained but cannot authorize or block those operations. Delivery requests
are not replayed after an ambiguous transport failure.

GSA connection setup is bounded to eight seconds within the original request
deadline. Recovery first refreshes system DNS; the existing external resolver
is used only if system resolution yields no usable public IPv4 address. It
retains the original route, Host/SNI and certificate verification, and never
replays a request after HTTP has acquired its connection. No IP is hardcoded,
no system DNS/proxy settings are changed, and trust checks are not disabled.

Validation used the actual GSA transport, native ADI helper, NAC exchange and
accepted-session iCloud quota query. Before the fix, injected TLS EOF recovery
failed after six seconds; afterward normal, injected EOF and injected connection
timeout cases succeeded with verified TLS (the timeout case recovered in about
nine seconds). Native ADI, NACSign and authenticated read-only quota retrieval
succeeded with deliberately poisoned inherited proxy variables removed by the
shared direct policy. These checks used private isolated native/NAC copies;
original account identity, session and acceptance checkpoint hashes were unchanged.
No registration, password login, new SMS or repeated account activation was used
for validation. Package tests and launcher tests passed; one unrelated registration
fixture exceeded its ten-second exit wait once, then the full dashboard package
passed on rerun without changing that fixture.

Deployment replaced only the panel and the binaries selected for subsequent
activation work. The five existing workers kept their PIDs and start times.
Panel readback confirmed the recent English and Chinese accounts remained
activated and online. The other three workers had already entered paused
`saved_session_check` renewal failures on earlier dates: their accepted
checkpoints and push connections remained present, but authenticated=false.
Those pre-existing renewal states were not restarted or modified by this task.
