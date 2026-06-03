import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { EmptyState, RecordEmptyState } from "./EmptyStates";

describe("EmptyState", () => {
  it("renders the title and fires the primary action", async () => {
    const onAction = vi.fn();
    render(
      <EmptyState
        title="No deals yet"
        description="Create your first deal."
        actionLabel="Create your first deal"
        onAction={onAction}
      />,
    );
    expect(screen.getByText("No deals yet")).toBeInTheDocument();
    await userEvent.click(
      screen.getByRole("button", { name: /Create your first deal/i }),
    );
    expect(onAction).toHaveBeenCalledTimes(1);
  });

  it("omits the CTA when no action is provided", () => {
    render(<EmptyState title="No matches" />);
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });
});

describe("RecordEmptyState", () => {
  it("uses the per-module copy for a known KType", () => {
    render(
      <RecordEmptyState ktype="crm.deal" ktypeName="Deal" onCreate={vi.fn()} />,
    );
    expect(screen.getByText(/No deals yet/i)).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Create your first deal/i }),
    ).toBeInTheDocument();
  });

  it("falls back to a generic message derived from the KType name", () => {
    render(
      <RecordEmptyState
        ktype="custom.widget"
        ktypeName="Widget"
        onCreate={vi.fn()}
      />,
    );
    expect(screen.getByText(/No widget yet/i)).toBeInTheDocument();
  });

  it("shows the import CTA only when onImport is provided", async () => {
    const onImport = vi.fn();
    render(
      <RecordEmptyState
        ktype="crm.contact"
        ktypeName="Contact"
        onCreate={vi.fn()}
        onImport={onImport}
      />,
    );
    await userEvent.click(screen.getByRole("button", { name: /Import data/i }));
    expect(onImport).toHaveBeenCalledTimes(1);
  });

  it("renders a message-only filter state and suppresses the module CTA when filterActive", () => {
    render(
      <RecordEmptyState
        ktype="crm.deal"
        ktypeName="Deal"
        filterActive
        onCreate={vi.fn()}
        onImport={vi.fn()}
      />,
    );
    // Filter-specific copy, not the misleading "No deals yet" CTA.
    expect(screen.getByText(/No matches for this view/i)).toBeInTheDocument();
    expect(screen.queryByText(/No deals yet/i)).not.toBeInTheDocument();
    // No create/import buttons in the filtered-empty state.
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });
});
