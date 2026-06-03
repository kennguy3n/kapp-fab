import { describe, it, expect, vi } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { MemoryRouter, Routes, Route, useNavigate } from "react-router-dom";
import { useCloseOnRouteChange } from "./useCloseOnRouteChange";

// Harness: a component that uses the hook and exposes navigate buttons so
// we can drive both in-app pushes and history back() the way the browser
// back button would.
function Harness({ close }: { close: () => void }) {
  useCloseOnRouteChange(close);
  const navigate = useNavigate();
  return (
    <div>
      <button onClick={() => navigate("/records")}>go-records</button>
      <button onClick={() => navigate("/dashboard")}>go-dashboard</button>
      <button onClick={() => navigate(-1)}>go-back</button>
    </div>
  );
}

function renderHarness(close: () => void) {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <Routes>
        <Route path="*" element={<Harness close={close} />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("useCloseOnRouteChange", () => {
  it("runs close once on mount", () => {
    const close = vi.fn();
    renderHarness(close);
    expect(close).toHaveBeenCalledTimes(1);
  });

  it("calls close on a forward (push) navigation", async () => {
    const close = vi.fn();
    renderHarness(close);
    close.mockClear();
    await act(async () => {
      screen.getByText("go-records").click();
    });
    expect(close).toHaveBeenCalledTimes(1);
  });

  it("calls close on browser back navigation", async () => {
    const close = vi.fn();
    renderHarness(close);
    // Build up history: / -> /records -> /dashboard
    await act(async () => screen.getByText("go-records").click());
    await act(async () => screen.getByText("go-dashboard").click());
    close.mockClear();
    // Browser back button: route changes without any in-app onClose.
    await act(async () => screen.getByText("go-back").click());
    expect(close).toHaveBeenCalledTimes(1);
  });

  it("does not call close again when the pathname is unchanged", async () => {
    const close = vi.fn();
    renderHarness(close);
    await act(async () => screen.getByText("go-records").click());
    close.mockClear();
    // Navigating to the same path is not a pathname change.
    await act(async () => screen.getByText("go-records").click());
    expect(close).not.toHaveBeenCalled();
  });
});
