---
name: claimy
description: Use when coordinating authorized work on a Claimy-managed sandbox or production environment by checking, acquiring, and releasing an environment claim.
---

# Claimy environment claims

Claims are advisory leases.
They are not deployment fencing or approval.
Use the normal authorization and deployment controls.

Choose the exact group, optional app, and environment.
Omit `--app` for a whole-group claim.
Use `sandbox` or `prod` deliberately.
Use `both` only when the work needs both environments.
A claim does not grant permission.

Use the CLI's existing login, `CLAIMY_ID_TOKEN`, or an explicit token file.
Browser login is user setup.
Ask the user to run `claimy auth login https://claimy.example.com` with the actual server URL when no login exists.
Never read, print, or extract tokens or OS keychain entries.

## Workflow

Replace uppercase placeholders in each command with the selected values.

1. Query the current view.
   Query does not lock an environment.

   ```sh
   claimy query --group GROUP --app APP --environments ENVIRONMENTS
   ```

2. Acquire atomically.
   Query results do not reserve an environment.
   Claims begin at database operation time and always expire.
   Without `--expires-at`, expiry is noon on the next calendar day in `Europe/Berlin`.
   A custom expiry must be RFC3339 with an explicit offset.

   Generate `UNIQUE_ACQUIRE_REQUEST_ID` once with `uuidgen`.
   Replace the placeholder and retain the value for retries.
   Retry the same logical request with the same ID and inputs.
   Use a new ID for a deliberate new attempt.

   ```sh
   claimy acquire --group GROUP --app APP --environments ENVIRONMENTS \
     --request-id UNIQUE_ACQUIRE_REQUEST_ID --output id
   ```

   Exit `0` means the claim is acquired and active.
   The output is the exact claim ID.
   Retain that ID for cleanup.
   Exit `1` means busy or an inactive successful replay.
   Exit `2` means an input, authentication, transport, storage, or response error.
   Stop before environment mutation on exit `1` or `2`.

3. Perform only the authorized work.
   Do not clear or extend another owner's claim without explicit approval.

4. Release the exact acquired claim during cleanup.
   Generate `UNIQUE_RELEASE_REQUEST_ID` once with `uuidgen`.
   Replace both placeholders and retain the request ID for release retries.

   ```sh
   claimy release --id ACQUIRED_CLAIM_ID --request-id UNIQUE_RELEASE_REQUEST_ID
   ```

   Release only the claim ID acquired for this work.
   Separate agent tool shells do not share variables.
   Retain request IDs and the acquired claim ID in agent context or caller-owned state.
   If cleanup is interrupted, the finite expiry is the fallback.
   Other successful CLI commands exit `0`.
   Their errors exit `2`.
