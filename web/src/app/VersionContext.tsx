import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';
import { api } from '../api/client';
import type { VersionInfo } from '../types/domain';

const fallbackVersion = import.meta.env.VITE_RELEASEDOCK_VERSION || '0.3.3';
const VersionContext = createContext<VersionInfo>({ version: fallbackVersion });

export function VersionProvider({ children }: { children: ReactNode }) {
  const [version, setVersion] = useState<VersionInfo>({ version: fallbackVersion });

  useEffect(() => {
    let active = true;
    api.version()
      .then((value) => {
        if (active && value?.version) setVersion(value);
      })
      .catch(() => {
        // The bundled version remains available while the API is starting or offline.
      });
    return () => {
      active = false;
    };
  }, []);

  const value = useMemo(() => version, [version]);
  return <VersionContext.Provider value={value}>{children}</VersionContext.Provider>;
}

export function useVersion(): VersionInfo {
  return useContext(VersionContext);
}
