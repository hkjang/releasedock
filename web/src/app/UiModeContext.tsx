import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';
import { api, type UiModeInfo } from '../api/client';
import { useAuth } from '../auth/AuthContext';

export type UiMode = 'simple' | 'full';

interface UiModeContextValue {
  mode: UiMode;
  loading: boolean;
  /** True when the administrator made simple mode the default for everyone. */
  systemDefault: UiMode;
  canUseSimple: boolean;
  canUseFull: boolean;
  /** Persists the choice so it survives the next login. */
  setMode: (mode: UiMode) => Promise<void>;
}

const UiModeContext = createContext<UiModeContextValue | null>(null);

const FALLBACK: UiModeInfo = {
  defaultUiMode: 'full',
  preferredUiMode: '',
  effectiveUiMode: 'full',
  canUseSimple: false,
  canUseFull: true,
  commandMode: 'PER_TARGET',
};

export function UiModeProvider({ children }: { children: ReactNode }) {
  const { user } = useAuth();
  const [info, setInfo] = useState<UiModeInfo>(FALLBACK);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (!user) {
      setInfo(FALLBACK);
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    api
      .uiMode()
      .then((value) => {
        if (!cancelled) setInfo(value);
      })
      // A missing or failing endpoint must not lock anyone out of the portal.
      .catch(() => {
        if (!cancelled) setInfo(FALLBACK);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [user]);

  const setMode = useCallback(async (mode: UiMode) => {
    setInfo((current) => ({ ...current, effectiveUiMode: mode, preferredUiMode: mode }));
    try {
      await api.setPreferredUiMode(mode);
    } catch {
      // The switch already applied locally; a failed save just means it will
      // not persist to the next session.
    }
  }, []);

  const value = useMemo<UiModeContextValue>(
    () => ({
      mode: info.effectiveUiMode,
      loading,
      systemDefault: info.defaultUiMode,
      canUseSimple: info.canUseSimple,
      canUseFull: info.canUseFull,
      setMode,
    }),
    [info, loading, setMode],
  );

  return <UiModeContext.Provider value={value}>{children}</UiModeContext.Provider>;
}

export function useUiMode(): UiModeContextValue {
  const value = useContext(UiModeContext);
  if (!value) throw new Error('useUiMode must be used inside UiModeProvider');
  return value;
}
