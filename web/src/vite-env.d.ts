/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_RELEASEDOCK_VERSION?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
