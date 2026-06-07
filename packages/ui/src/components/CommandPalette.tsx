import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  Modal,
  ModalContent,
  ModalTitle,
  ModalDescription,
} from "./Modal";
import { cn } from "../lib/cn";

/**
 * CommandPalette is a Cmd/Ctrl-K command launcher built from the
 * Modal primitive plus a filtered, keyboard-navigable list — no
 * `cmdk` dependency.  It's fully controlled (`open` / `onOpenChange`)
 * so the host owns the keyboard shortcut that toggles it and can
 * reopen it programmatically (e.g. from a header button).
 *
 * Items are grouped (nav, actions, recents…).  Typing filters every
 * group by the item label and any extra `keywords`; ↑/↓ move the
 * active item across group boundaries, Enter runs it, Escape closes
 * (handled by Radix Dialog).  The active item is scrolled into view
 * so long lists stay usable.
 */
export interface CommandItem {
  id: string;
  label: string;
  /** Right-aligned hint, e.g. a shortcut or section name. */
  hint?: ReactNode;
  icon?: ReactNode;
  /** Extra terms to match against beyond the visible label. */
  keywords?: string[];
  onSelect: () => void;
}

export interface CommandGroup {
  heading?: string;
  items: CommandItem[];
}

export interface CommandPaletteProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  groups: CommandGroup[];
  placeholder?: string;
  emptyMessage?: ReactNode;
}

function matches(item: CommandItem, query: string): boolean {
  if (!query) return true;
  const q = query.toLowerCase();
  if (item.label.toLowerCase().includes(q)) return true;
  return (item.keywords ?? []).some((k) => k.toLowerCase().includes(q));
}

export function CommandPalette({
  open,
  onOpenChange,
  groups,
  placeholder = "Type a command or search…",
  emptyMessage = "No results found.",
}: CommandPaletteProps) {
  const [query, setQuery] = useState("");
  const [activeIndex, setActiveIndex] = useState(0);
  const listRef = useRef<HTMLDivElement>(null);

  // Filtered groups + a parallel flat list so keyboard nav can move
  // across group boundaries with a single index.
  const { visibleGroups, flatItems, indexById } = useMemo(() => {
    const vg: CommandGroup[] = [];
    const flat: CommandItem[] = [];
    // Stable id -> flat index map so keyboard nav and the active-row
    // highlight share one ordering without a mutable render counter.
    const idx = new Map<string, number>();
    for (const group of groups) {
      const items = group.items.filter((it) => matches(it, query));
      if (items.length > 0) {
        vg.push({ heading: group.heading, items });
        for (const it of items) {
          idx.set(it.id, flat.length);
          flat.push(it);
        }
      }
    }
    return { visibleGroups: vg, flatItems: flat, indexById: idx };
  }, [groups, query]);

  // Reset query + selection each time the palette opens.
  useEffect(() => {
    if (open) {
      setQuery("");
      setActiveIndex(0);
    }
  }, [open]);

  // Clamp the active index whenever the filtered list shrinks.
  useEffect(() => {
    setActiveIndex((i) => Math.min(i, Math.max(0, flatItems.length - 1)));
  }, [flatItems.length]);

  // Keep the active row scrolled into view.
  useEffect(() => {
    const el = listRef.current?.querySelector<HTMLElement>(
      `[data-command-index="${activeIndex}"]`,
    );
    el?.scrollIntoView({ block: "nearest" });
  }, [activeIndex]);

  const run = (item: CommandItem) => {
    onOpenChange(false);
    item.onSelect();
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActiveIndex((i) => (flatItems.length ? (i + 1) % flatItems.length : 0));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActiveIndex((i) =>
        flatItems.length ? (i - 1 + flatItems.length) % flatItems.length : 0,
      );
    } else if (e.key === "Enter") {
      e.preventDefault();
      const item = flatItems[activeIndex];
      if (item) run(item);
    }
  };

  return (
    <Modal open={open} onOpenChange={onOpenChange}>
      <ModalContent
        className="max-w-xl p-0 gap-0 top-[20%] translate-y-0"
        onKeyDown={onKeyDown}
        aria-label="Command palette"
      >
        <ModalTitle className="sr-only">Command palette</ModalTitle>
        {/* Visually-hidden description satisfies Radix Dialog's
            aria-describedby expectation and silences its dev-mode
            "Missing Description" console warning. */}
        <ModalDescription className="sr-only">
          Search and run commands, navigate to pages, or create records.
        </ModalDescription>
        <div className="flex items-center gap-2 border-b border-border px-4">
          <svg
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
            className="h-4 w-4 shrink-0 text-fg-subtle"
            aria-hidden="true"
          >
            <circle cx="11" cy="11" r="8" />
            <line x1="21" y1="21" x2="16.65" y2="16.65" />
          </svg>
          <input
            autoFocus
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder={placeholder}
            aria-label="Search commands"
            className="h-12 w-full bg-transparent text-sm text-fg outline-none placeholder:text-fg-subtle"
          />
        </div>
        <div
          ref={listRef}
          role="listbox"
          aria-label="Commands"
          className="max-h-80 overflow-y-auto p-2"
        >
          {flatItems.length === 0 ? (
            <p className="px-3 py-6 text-center text-sm text-fg-muted">
              {emptyMessage}
            </p>
          ) : (
            visibleGroups.map((group, gi) => (
              <div key={gi} className="mb-1 last:mb-0">
                {group.heading && (
                  <p className="px-2 py-1.5 text-xs font-medium text-fg-subtle">
                    {group.heading}
                  </p>
                )}
                {group.items.map((item) => {
                  const index = indexById.get(item.id) ?? 0;
                  const active = index === activeIndex;
                  return (
                    <button
                      key={item.id}
                      type="button"
                      data-command-index={index}
                      role="option"
                      aria-selected={active}
                      onMouseMove={() => setActiveIndex(index)}
                      onClick={() => run(item)}
                      className={cn(
                        "flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-left text-sm",
                        active ? "bg-bg-subtle text-fg" : "text-fg-muted",
                      )}
                    >
                      {item.icon && (
                        <span className="shrink-0 text-fg-subtle [&_svg]:h-4 [&_svg]:w-4">
                          {item.icon}
                        </span>
                      )}
                      <span className="flex-1 truncate text-fg">
                        {item.label}
                      </span>
                      {item.hint && (
                        <span className="shrink-0 text-xs text-fg-subtle">
                          {item.hint}
                        </span>
                      )}
                    </button>
                  );
                })}
              </div>
            ))
          )}
        </div>
      </ModalContent>
    </Modal>
  );
}
