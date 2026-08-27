import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';
import { api, ApiError } from '../api/client';
import type { User } from '../types/domain';

interface AuthContextValue {
  user?: User;
  loading: boolean;
  login: (username: string, password: string) => Promise<User>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
  isAdmin: boolean;
  hasPermission: (permission: string) => boolean;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User>();
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      setUser(await api.me());
    } catch (error) {
      if (error instanceof ApiError && (error.status === 401 || error.status === 403)) {
        setUser(undefined);
      } else {
        setUser(undefined);
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
    return result.user;
  }, []);

  const logout = useCallback(async () => {
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
      user,
      loading,
      login,
      logout,
      refresh,
      isAdmin: Boolean(user?.roles.some((role) => role.toLowerCase() === 'admin') || user?.permissions?.some((permission) => permission.startsWith('admin.'))),
      hasPermission,
    }),
    [user, loading, login, logout, refresh, hasPermission],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const value = useContext(AuthContext);
  if (!value) throw new Error('useAuth must be used inside AuthProvider');
  return value;
}
