import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { AuthConfig } from '../api/client';
import { clearSilentSsoState, markSignedOut, shouldAttemptSilentSso, silentSsoAttempted } from './silentSso';

const enabled: AuthConfig = { local_enabled: true, oidc: { enabled: true, autoLogin: true } };

function atPath(search: string) {
  window.history.replaceState({}, '', `/${search}`);
}

describe('silent SSO', () => {
  beforeEach(() => {
    window.sessionStorage.clear();
    atPath('');
  });
  afterEach(() => {
    window.sessionStorage.clear();
  });

  it('attempts once when the administrator enabled auto-login', () => {
    expect(shouldAttemptSilentSso(enabled)).toBe(true);
  });

  it('does nothing when auto-login is off', () => {
    expect(shouldAttemptSilentSso({ local_enabled: true, oidc: { enabled: true, autoLogin: false } })).toBe(false);
  });

  it('does nothing when OIDC itself is disabled', () => {
    expect(shouldAttemptSilentSso({ local_enabled: true, oidc: { enabled: false, autoLogin: true } })).toBe(false);
  });

  // The whole risk of prompt=none is bouncing the browser forever.
  it('does not retry after an attempt was already made', () => {
    window.sessionStorage.setItem('releasedock.sso.silentAttempted', 'true');
    expect(shouldAttemptSilentSso(enabled)).toBe(false);
  });

  it('does not retry when the provider reported no session', () => {
    atPath('?sso=none');
    expect(shouldAttemptSilentSso(enabled)).toBe(false);
  });

  it('does not retry when the provider reported an error', () => {
    atPath('?sso=error');
    expect(shouldAttemptSilentSso(enabled)).toBe(false);
  });

  // Signing out on purpose must stick even though Keycloak still has a session.
  it('stays suppressed after an explicit sign-out', () => {
    markSignedOut();
    expect(shouldAttemptSilentSso(enabled)).toBe(false);
    expect(silentSsoAttempted()).toBe(true);
  });

  it('becomes eligible again once a session is established', () => {
    markSignedOut();
    expect(shouldAttemptSilentSso(enabled)).toBe(false);
    clearSilentSsoState();
    expect(shouldAttemptSilentSso(enabled)).toBe(true);
  });
});
