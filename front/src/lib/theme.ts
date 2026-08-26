import { createContext, createElement, useCallback, useContext, useEffect, useState, type ReactNode } from 'react';
import { flushSync } from 'react-dom';

type Theme = 'dark' | 'light';

const STORAGE_KEY = 'uc-theme';

function readInitialTheme(): Theme {
  if (typeof document === 'undefined') return 'dark';
  const attr = document.documentElement.dataset.theme;
  return attr === 'light' ? 'light' : 'dark';
}

interface ThemeContextValue {
  theme: Theme;
  toggleTheme: () => void;
  /** Direct set — for external theme sources (Telegram Mini App sync). */
  setTheme: (theme: Theme) => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setTheme] = useState<Theme>(readInitialTheme);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem(STORAGE_KEY, theme);
    // Keep the browser/OS chrome on the app's theme (same values as the
    // pre-paint script in index.html — --bg-base per theme).
    const meta = document.querySelector<HTMLMetaElement>('meta[name="theme-color"]');
    if (meta) meta.content = theme === 'light' ? '#f2f2ef' : '#16181b';
  }, [theme]);

  // Write the attribute inside the callback: from the effect alone the
  // transition snapshots identical frames and the real change still snaps.
  const toggleTheme = useCallback(() => {
    const next: Theme = theme === 'dark' ? 'light' : 'dark';
    const reduce = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
    if (!document.startViewTransition || reduce) {
      setTheme(next);
      return;
    }
    document.startViewTransition(() => {
      document.documentElement.dataset.theme = next;
      flushSync(() => setTheme(next));
    });
  }, [theme]);

  return createElement(ThemeContext.Provider, { value: { theme, toggleTheme, setTheme } }, children);
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error('useTheme must be used within a ThemeProvider');
  return ctx;
}
