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
