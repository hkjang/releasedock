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

// jsdom does not lay anything out and so implements no scrolling. A view that
// keeps its newest log line in sight calls this on every update, which throws
// and takes the whole page down before a test can assert anything about it.
Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: () => {} });

beforeEach(() => {
  window.localStorage.clear();
  document.cookie = 'releasedock_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/';
  window.history.replaceState({}, '', '/');
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
