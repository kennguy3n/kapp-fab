import { useSyncExternalStore } from "react";

/**
 * Theme controller — owns the light/dark colour scheme for the whole
 * app.  The design system (packages/ui/src/styles/globals.css) defines
 * `.dark` token overrides; this module is the single place that
 * decides when that class is on `<html>`.
 *
 * Resolution order on first load:
 *   1. an explicit user choice persisted in localStorage, else
 *   2. the OS `prefers-color-scheme`.
 * While the user has NOT made an explicit choice we keep following the
 * OS setting live; once they toggle, their choice wins until cleared.
 *
 * Call `initTheme()` once before React renders (main.tsx) to apply the
 * resolved scheme synchronously and avoid a flash of the wrong theme.
 * Components read/observe the scheme via `useTheme()`.
 */
export type Theme = "light" | "dark";

const STORAGE_KEY = "kapp.theme";

function getSystemTheme(): Theme {
  if (typeof window === "undefined" || !window.matchMedia) return "light";
  return window.matchMedia("(prefers-color-scheme: dark)").matches
    ? "dark"
    : "light";
}

function readStored(): Theme | null {
  if (typeof localStorage === "undefined") return null;
  const v = localStorage.getItem(STORAGE_KEY);
  return v === "light" || v === "dark" ? v : null;
}

export function resolveInitialTheme(): Theme {
  return readStored() ?? getSystemTheme();
}

function apply(theme: Theme): void {
  if (typeof document === "undefined") return;
  document.documentElement.classList.toggle("dark", theme === "dark");
}

const listeners = new Set<() => void>();
let current: Theme = "light";
let initialized = false;

function notify(): void {
  for (const l of listeners) l();
}

function ensureInit(): void {
  if (initialized) return;
  initialized = true;
  current = resolveInitialTheme();
  apply(current);
  if (typeof window !== "undefined" && window.matchMedia) {
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    mq.addEventListener("change", (e) => {
      // An explicit user choice overrides the OS preference.
      if (readStored() !== null) return;
      current = e.matches ? "dark" : "light";
      apply(current);
      notify();
    });
  }
}

export function initTheme(): void {
  ensureInit();
}

export function getTheme(): Theme {
  ensureInit();
  return current;
}

export function setTheme(theme: Theme): void {
  ensureInit();
  current = theme;
  if (typeof localStorage !== "undefined") {
    localStorage.setItem(STORAGE_KEY, theme);
  }
  apply(theme);
  notify();
}

export function toggleTheme(): void {
  setTheme(getTheme() === "dark" ? "light" : "dark");
}

function subscribe(cb: () => void): () => void {
  listeners.add(cb);
  return () => {
    listeners.delete(cb);
  };
}

export interface UseThemeResult {
  theme: Theme;
  setTheme: (theme: Theme) => void;
  toggle: () => void;
}

export function useTheme(): UseThemeResult {
  const theme = useSyncExternalStore(
    subscribe,
    getTheme,
    (): Theme => "light",
  );
  return { theme, setTheme, toggle: toggleTheme };
}
