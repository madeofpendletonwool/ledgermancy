import type { PlaidLinkError, PlaidLinkOnExitMetadata } from 'react-plaid-link'

/**
 * Turns a Plaid Link failure into something a person can act on.
 *
 * Link's own failure screen says "Something went wrong" and nothing else, and
 * the obvious handler — `display_message ?? error_message` — often reproduces
 * exactly that, because Plaid leaves both null for institution-side faults. A
 * user who hits one is then told nothing, and neither is whoever they report it
 * to: the failure happens entirely inside the widget, so the backend logs are
 * silent too. The only durable identifiers are on the error and its metadata.
 *
 * So the code and the session id are always included:
 *
 *   - error_code (INSTITUTION_DOWN, PRODUCTS_NOT_SUPPORTED, ...) is the single
 *     most useful token, and is what Plaid's own docs are indexed by.
 *   - link_session_id is what the Plaid Dashboard's Link session log is keyed
 *     on, and that log holds the full server-side trace of the attempt. Without
 *     it there is no way back from "it failed" to what actually happened.
 */
export function describePlaidExit(
  err: PlaidLinkError,
  metadata: PlaidLinkOnExitMetadata,
): string {
  const headline =
    err.display_message ||
    err.error_message ||
    'Plaid could not complete the connection.'

  const details = [
    err.error_code && `code ${err.error_code}`,
    metadata.institution?.name,
    metadata.link_session_id && `session ${metadata.link_session_id}`,
  ].filter(Boolean)

  return details.length > 0 ? `${headline} (${details.join(' · ')})` : headline
}

/**
 * Records the full failure to the console.
 *
 * The on-screen message stays short enough to read; this keeps everything else
 * — error_type, the exit status, the request id — somewhere it can be copied
 * out of when a report needs to go to Plaid support.
 */
export function logPlaidExit(
  err: PlaidLinkError,
  metadata: PlaidLinkOnExitMetadata,
): void {
  console.error('plaid link failed', {
    error_type: err.error_type,
    error_code: err.error_code,
    error_message: err.error_message,
    display_message: err.display_message,
    institution: metadata.institution,
    status: metadata.status,
    link_session_id: metadata.link_session_id,
    request_id: metadata.request_id,
  })
}
