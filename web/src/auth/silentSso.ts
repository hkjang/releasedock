import { api, type AuthConfig } from '../api/client';

// sessionStorage rather than localStorage: a silent attempt is scoped to this
// tab's browsing session, so opening a fresh tab tries again while a reload
// after a refusal does not.
const ATTEMPTED_KEY = 'releasedock.sso.silentAttempted';
const SIGNED_OUT_KEY = 'releasedock.sso.signedOut';

function readFlag(key: string): boolean {
  try {
    return window.sessionStorage.getItem(key) === 'true';
  } catch {
    // Private modes and blocked site data throw; treating that as "already
    // attempted" is the safe answer because it prevents a redirect loop.
    return true;
  }
}

function writeFlag(key: string, value: boolean) {
  try {
    if (value) window.sessionStorage.setItem(key, 'true');
    else window.sessionStorage.removeItem(key);
  } catch {
    /* nothing to do; the guard above already fails closed */
  }
}

/** Records that the user signed out on purpose, which suppresses auto-login. */
export function markSignedOut() {
  writeFlag(SIGNED_OUT_KEY, true);
  writeFlag(ATTEMPTED_KEY, true);
}

/** Clears the suppression once a session exists again. */
export function clearSilentSsoState() {
  writeFlag(SIGNED_OUT_KEY, false);
  writeFlag(ATTEMPTED_KEY, false);
}

export function silentSsoAttempted(): boolean {
  return readFlag(ATTEMPTED_KEY);
}

/**
 * Decides whether to try signing in without showing a login screen.
 *
 * It must never run more than once per browsing session: prompt=none either
 * answers immediately or redirects back with login_required, and retrying that
 * on every page load would bounce the browser in a loop.
 */
export function shouldAttemptSilentSso(config: AuthConfig): boolean {
  if (!config.oidc?.enabled || !config.oidc?.autoLogin) return false;
  if (readFlag(SIGNED_OUT_KEY)) return false;
  if (readFlag(ATTEMPTED_KEY)) return false;
  // The callback appends this marker when the provider had no session, so a
  // refusal is remembered even if sessionStorage was cleared in between.
  const params = new URLSearchParams(window.location.search);
  if (params.get('sso') === 'none' || params.get('sso') === 'error') return false;
  return true;
}

/** Sends the browser to the provider for a silent attempt. */
export function beginSilentSso(returnTo: string) {
  writeFlag(ATTEMPTED_KEY, true);
  const safe = returnTo.startsWith('/') && !returnTo.startsWith('//') ? returnTo : '/';
  window.location.assign(
    `/api/v1/auth/oidc/login?prompt=none&return_to=${encodeURIComponent(safe)}`,
  );
}

export async function loadAuthConfig(): Promise<AuthConfig | undefined> {
  try {
    return await api.authConfig();
  } catch {
    return undefined;
  }
}
