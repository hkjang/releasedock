import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/react';
import { afterEach, beforeEach, vi } from 'vitest';

const storage = (() => {
  let values = new Map<string, string>();
  return {
    get length() { return values.size; },
    clear: () => values.clear(),
    getItem: (key: string) => values.get(key) ?? null,
    key: (index: number) => [...values.keys()][index] ?? null,
    removeItem: (key: string) => { values.delete(key); },
    setItem: (key: string, value: string) => { values.set(key, String(value)); },
  } satisfies Storage;
})();

Object.defineProperty(window, 'localStorage', { configurable: true, value: storage });

beforeEach(() => {
  window.localStorage.clear();
  document.cookie = 'releasedock_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/';
  window.history.replaceState({}, '', '/');
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
