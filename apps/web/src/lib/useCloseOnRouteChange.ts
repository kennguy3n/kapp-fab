import { useEffect, useRef } from "react";
import { useLocation } from "react-router-dom";

/**
 * Invoke `close` whenever the route pathname changes. Used to dismiss
 * transient overlays (e.g. the mobile nav sheet) on ANY navigation —
 * including browser back/forward, which change the route WITHOUT firing
 * an in-app onClose handler and would otherwise leave the overlay on top
 * of the new page.
 *
 * The latest `close` is held in a ref so the effect can depend only on
 * the pathname: callers typically pass an inline closure (a new identity
 * every render), and depending on it directly would re-run the effect on
 * every render — instantly re-closing the overlay and making it
 * impossible to open. `close` should tolerate being called when nothing
 * is open (it runs once on mount too).
 */
export function useCloseOnRouteChange(close: () => void): void {
  const { pathname } = useLocation();
  const closeRef = useRef(close);
  closeRef.current = close;
  useEffect(() => {
    closeRef.current();
  }, [pathname]);
}
