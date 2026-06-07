import { useEffect, useId, useState, type ReactNode } from "react";
import {
  Modal,
  ModalContent,
  ModalHeader,
  ModalTitle,
  ModalDescription,
  ModalFooter,
} from "./Modal";
import { Button } from "./Button";
import { Input } from "./Input";

/**
 * PromptDialog is the accessible replacement for `window.prompt()`.
 * It's a controlled modal wrapping a single text Input.  The host
 * owns `open`; on submit the entered value is handed back through
 * `onSubmit` and the host decides whether to close.
 *
 * The input is seeded from `defaultValue` each time the dialog
 * opens (so reopening doesn't leak the previous edit), submits on
 * Enter, and — when `required` (the default) — disables the submit
 * button while empty so the caller never receives a blank string.
 */
export interface PromptDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  /** Field label, associated to the input for screen readers. */
  label?: ReactNode;
  placeholder?: string;
  defaultValue?: string;
  confirmLabel?: string;
  cancelLabel?: string;
  /** Require a non-empty, non-whitespace value. Defaults to true. */
  required?: boolean;
  loading?: boolean;
  onSubmit: (value: string) => void;
}

export function PromptDialog({
  open,
  onOpenChange,
  title,
  description,
  label,
  placeholder,
  defaultValue = "",
  confirmLabel = "Save",
  cancelLabel = "Cancel",
  required = true,
  loading = false,
  onSubmit,
}: PromptDialogProps) {
  const [value, setValue] = useState(defaultValue);
  const inputId = useId();

  // Reset to the seed value whenever the dialog transitions open so
  // a reopened prompt doesn't show the prior session's edit.
  useEffect(() => {
    if (open) setValue(defaultValue);
  }, [open, defaultValue]);

  const empty = value.trim().length === 0;
  const canSubmit = !loading && (!required || !empty);

  const submit = () => {
    if (!canSubmit) return;
    onSubmit(value);
  };

  return (
    <Modal open={open} onOpenChange={onOpenChange}>
      <ModalContent className="max-w-md">
        <form
          onSubmit={(e) => {
            e.preventDefault();
            submit();
          }}
        >
          <ModalHeader>
            <ModalTitle>{title}</ModalTitle>
            {description && <ModalDescription>{description}</ModalDescription>}
          </ModalHeader>
          {label && (
            <label
              htmlFor={inputId}
              className="mb-1.5 block text-sm font-medium text-fg"
            >
              {label}
            </label>
          )}
          <Input
            id={inputId}
            autoFocus
            value={value}
            placeholder={placeholder}
            onChange={(e) => setValue(e.target.value)}
            disabled={loading}
          />
          <ModalFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={loading}
            >
              {cancelLabel}
            </Button>
            <Button type="submit" disabled={!canSubmit}>
              {loading ? "Working…" : confirmLabel}
            </Button>
          </ModalFooter>
        </form>
      </ModalContent>
    </Modal>
  );
}
