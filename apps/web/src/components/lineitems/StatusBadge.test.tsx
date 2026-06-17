import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { StatusBadge } from "./StatusBadge";

describe("StatusBadge", () => {
  it("renders the humanised status label", () => {
    render(<StatusBadge status="fulfilled" />);
    expect(screen.getByText("Fulfilled")).toBeInTheDocument();
  });

  it("title-cases multi-word statuses", () => {
    render(<StatusBadge status="partially_received" />);
    expect(screen.getByText("Partially Received")).toBeInTheDocument();
  });

  it("falls back gracefully for unknown statuses", () => {
    render(<StatusBadge status="weird_state" />);
    expect(screen.getByText("Weird State")).toBeInTheDocument();
  });
});
