import { useEffect, useSyncExternalStore, type ReactNode } from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "../lib/cn";

/**
 * Toast is a lightweight, dependency-free notification system in the
 * shape of `sonner`'s imperative API.  A module-level pub/sub store
 * holds the active toasts; the exported `toast` object pushes into
 * it from anywhere (including outside React — e.g. a React Query
 * mutation callback), and a single `<Toaster />` mounted at the app
 * root subscribes and renders the overlay.
 *
 * Building this in-house (rather than pulling `sonner`) keeps the
 * design tokens, motion, and a11y wiring consistent with the rest
 * of @kapp/ui and avoids a runtime dependency for ~80 lines of
 * code.  It replaces every `window.alert()` call in the app.
 */
export type ToastVariant = "default" | "success" | "error" | "warning" | "info";

export interface ToastItem {
  id: string;
  variant: ToastVariant;
  title: ReactNode;
  description?: ReactNode;
  /** Auto-dismiss delay in ms.  `0` keeps the toast until dismissed. */
  duration: number;
}

export interface ToastOptions {
  description?: ReactNode;
  duration?: number;
  id?: string;
}

type Listener = (toasts: ToastItem[]) => void;

const DEFAULT_DURATION = 4000;

class ToastStore {
  private toasts: ToastItem[] = [];
  private listeners = new Set<Listener>();

  subscribe = (listener: Listener): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  getSnapshot = (): ToastItem[] => this.toasts;

  private emit() {
    for (const l of this.listeners) l(this.toasts);
  }

  add(variant: ToastVariant, title: ReactNode, options?: ToastOptions): string {
    const id =
      options?.id ??
      `toast-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 7)}`;
    const item: ToastItem = {
      id,
      variant,
      title,
      description: options?.description,
      duration: options?.duration ?? DEFAULT_DURATION,
    };
    // De-dupe by id: replace if an id was supplied and already shown.
    this.toasts = [...this.toasts.filter((t) => t.id !== id), item];
    this.emit();
    return id;
  }

  dismiss(id: string) {
    this.toasts = this.toasts.filter((t) => t.id !== id);
    this.emit();
  }
}

const store = new ToastStore();

/**
 * Imperative entry point.  `toast.success("Saved")`,
 * `toast.error("Failed", { description: err.message })`, etc.
 */
export const toast = {
  show: (title: ReactNode, options?: ToastOptions) =>
    store.add("default", title, options),
  success: (title: ReactNode, options?: ToastOptions) =>
    store.add("success", title, options),
  error: (title: ReactNode, options?: ToastOptions) =>
    store.add("error", title, options),
  warning: (title: ReactNode, options?: ToastOptions) =>
    store.add("warning", title, options),
  info: (title: ReactNode, options?: ToastOptions) =>
    store.add("info", title, options),
  dismiss: (id: string) => store.dismiss(id),
};

/** Hook form for components that prefer dependency-injected access. */
export function useToast() {
  return toast;
}

const toastVariants = cva(
  cn(
    "pointer-events-auto w-full max-w-sm overflow-hidden rounded-lg border p-4 shadow-lg",
    "flex items-start gap-3",
    "data-[state=open]:animate-in data-[state=open]:fade-in-0 data-[state=open]:slide-in-from-bottom-2",
  ),
  {
    variants: {
      variant: {
        default: "border-border bg-bg-elevated text-fg",
        success: "border-success/30 bg-bg-elevated text-fg",
        error: "border-danger/30 bg-bg-elevated text-fg",
        warning: "border-warning/40 bg-bg-elevated text-fg",
        info: "border-info/30 bg-bg-elevated text-fg",
      },
    },
    defaultVariants: { variant: "default" },
  },
);

const iconColor: Record<ToastVariant, string> = {
  default: "text-fg-subtle",
  success: "text-success",
  error: "text-danger",
  warning: "text-warning",
  info: "text-info",
};

function ToastIcon({ variant }: { variant: ToastVariant }) {
  const common = {
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 2,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    className: cn("h-5 w-5 shrink-0", iconColor[variant]),
    "aria-hidden": true,
  };
  switch (variant) {
    case "success":
      return (
        <svg {...common}>
          <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14" />
          <polyline points="22 4 12 14.01 9 11.01" />
        </svg>
      );
    case "error":
      return (
        <svg {...common}>
          <circle cx="12" cy="12" r="10" />
          <line x1="15" y1="9" x2="9" y2="15" />
          <line x1="9" y1="9" x2="15" y2="15" />
        </svg>
      );
    case "warning":
      return (
        <svg {...common}>
          <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
          <line x1="12" y1="9" x2="12" y2="13" />
          <line x1="12" y1="17" x2="12.01" y2="17" />
        </svg>
      );
    case "info":
      return (
        <svg {...common}>
          <circle cx="12" cy="12" r="10" />
          <line x1="12" y1="16" x2="12" y2="12" />
          <line x1="12" y1="8" x2="12.01" y2="8" />
        </svg>
      );
    default:
      return null;
  }
}

function ToastRow({ item }: { item: ToastItem }) {
  useEffect(() => {
    if (item.duration <= 0) return;
    const timer = setTimeout(() => store.dismiss(item.id), item.duration);
    return () => clearTimeout(timer);
  }, [item.id, item.duration]);

  return (
    <div
      data-state="open"
      role={item.variant === "error" ? "alert" : "status"}
      aria-live={item.variant === "error" ? "assertive" : "polite"}
      className={toastVariants({ variant: item.variant })}
    >
      <ToastIcon variant={item.variant} />
      <div className="flex-1 min-w-0">
        <p className="text-sm font-medium">{item.title}</p>
        {item.description && (
          <p className="mt-0.5 text-sm text-fg-muted">{item.description}</p>
        )}
      </div>
      <button
        type="button"
        onClick={() => store.dismiss(item.id)}
        aria-label="Dismiss notification"
        className="shrink-0 rounded-sm text-fg-subtle transition-colors hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
      >
        <svg
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
          className="h-4 w-4"
          aria-hidden="true"
        >
          <line x1="18" y1="6" x2="6" y2="18" />
          <line x1="6" y1="6" x2="18" y2="18" />
        </svg>
      </button>
    </div>
  );
}

export interface ToasterProps {
  /** Corner placement. Defaults to bottom-right. */
  position?:
    | "top-left"
    | "top-right"
    | "bottom-left"
    | "bottom-right"
    | "top-center"
    | "bottom-center";
}

const positionClass: Record<NonNullable<ToasterProps["position"]>, string> = {
  "top-left": "top-0 left-0 items-start",
  "top-right": "top-0 right-0 items-end",
  "bottom-left": "bottom-0 left-0 items-start",
  "bottom-right": "bottom-0 right-0 items-end",
  "top-center": "top-0 left-1/2 -translate-x-1/2 items-center",
  "bottom-center": "bottom-0 left-1/2 -translate-x-1/2 items-center",
};

/**
 * Toaster is the overlay region.  Mount exactly one at the app root.
 * It's a fixed, pointer-events-none stack so it never blocks the UI
 * except on the toasts themselves.
 */
export function Toaster({ position = "bottom-right" }: ToasterProps) {
  const toasts = useSyncExternalStore(
    store.subscribe,
    store.getSnapshot,
    store.getSnapshot,
  );

  return (
    <div
      className={cn(
        "pointer-events-none fixed z-[100] flex w-full max-w-sm flex-col gap-2 p-4",
        positionClass[position],
      )}
    >
      {toasts.map((item) => (
        <ToastRow key={item.id} item={item} />
      ))}
    </div>
  );
}

export { toastVariants };
export type ToastVariantProps = VariantProps<typeof toastVariants>;
