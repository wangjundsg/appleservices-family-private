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

The pre-existing ambiguous receipt was not migrated: automatic approval blocked
the proposed receipt reconciliation and activation recovery command before it
executed. This account therefore remains registered, awaiting activation handoff;
successful live activation has not been verified.
