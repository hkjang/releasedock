import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';
import { api, ApiError } from '../api/client';
import { beginSilentSso, clearSilentSsoState, loadAuthConfig, markSignedOut, shouldAttemptSilentSso } from './silentSso';
import type { User } from '../types/domain';

interface AuthContextValue {
  user?: User;
  loading: boolean;
  login: (username: string, password: string) => Promise<User>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
  isAdmin: boolean;
  /** True while the browser is being redirected for a silent SSO attempt. */
  attemptingSso: boolean;
  hasPermission: (permission: string) => boolean;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User>();
  const [loading, setLoading] = useState(true);

  // A silent SSO attempt is made once, before anything renders, so a visitor
  // with a live Keycloak session never sees the login screen at all.
  const [attemptingSso, setAttemptingSso] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const current = await api.me();
      setUser(current);
      clearSilentSsoState();
    } catch (error) {
      setUser(undefined);
      // Only an unauthenticated visitor is a candidate; a permission
      // failure means the session is fine and must not trigger a redirect.
      if (error instanceof ApiError && error.status === 401) {
        const config = await loadAuthConfig();
        if (config && shouldAttemptSilentSso(config)) {
          setAttemptingSso(true);
          beginSilentSso(`${window.location.pathname}${window.location.search}`);
          return;
        }
      }
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const login = useCallback(async (username: string, password: string) => {
    const result = await api.login(username, password);
    setUser(result.user);
    clearSilentSsoState();
    return result.user;
  }, []);

  const logout = useCallback(async () => {
    // An explicit sign-out must stick even while the Keycloak session is
    // still alive, otherwise auto-login would immediately undo it.
    markSignedOut();
    try {
      await api.logout();
    } finally {
      setUser(undefined);
    }
  }, []);

  const hasPermission = useCallback(
    (permission: string) => Boolean(user?.permissions?.includes(permission)),
    [user],
  );

  const value = useMemo<AuthContextValue>(
    () => ({
      attemptingSso,
      user,
      loading,
      login,
      logout,
      refresh,
      isAdmin: Boolean(user?.roles.some((role) => role.toLowerCase() === 'admin') || user?.permissions?.some((permission) => permission.startsWith('admin.'))),
      hasPermission,
    }),
    [user, loading, attemptingSso, login, logout, refresh, hasPermission],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const value = useContext(AuthContext);
  if (!value) throw new Error('useAuth must be used inside AuthProvider');
  return value;
}
