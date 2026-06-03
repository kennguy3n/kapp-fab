import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, act, waitFor } from "@testing-library/react";

const countQueue = vi.fn<() => Promise<number>>();
let queueListener: (() => void) | null = null;
const subscribeQueue = vi.fn((listener: () => void) => {
  queueListener = listener;
  return () => {
    queueListener = null;
  };
});

vi.mock("../lib/offlineQueue", () => ({
  countQueue: () => countQueue(),
  subscribeQueue: (listener: () => void) => subscribeQueue(listener),
}));

import { OfflineIndicator } from "./OfflineIndicator";

function setOnline(value: boolean) {
  Object.defineProperty(navigator, "onLine", {
    value,
    configurable: true,
    writable: true,
  });
}

describe("OfflineIndicator", () => {
  beforeEach(() => {
    countQueue.mockReset();
    countQueue.mockResolvedValue(0);
    subscribeQueue.mockClear();
    queueListener = null;
    setOnline(true);
  });

  afterEach(() => {
    setOnline(true);
  });

  it("renders nothing when online with an empty queue", async () => {
    const { container } = render(<OfflineIndicator />);
    // Let the initial countQueue() resolve.
    await act(async () => {});
    expect(container).toBeEmptyDOMElement();
  });

  it("shows an offline banner when connectivity drops", async () => {
    setOnline(false);
    render(<OfflineIndicator />);
    await act(async () => {});

    const banner = screen.getByRole("status");
    expect(banner).toHaveTextContent(/you're offline/i);
    expect(banner).toHaveAttribute("data-online", "false");
  });

  it("surfaces the queued mutation count while offline", async () => {
    setOnline(false);
    countQueue.mockResolvedValue(2);
    render(<OfflineIndicator />);

    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent(
        /2 changes queued/i,
      ),
    );
  });

  it("singularises the queued-change label", async () => {
    setOnline(false);
    countQueue.mockResolvedValue(1);
    render(<OfflineIndicator />);

    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent(/1 change queued/i),
    );
  });

  it("shows a syncing banner when back online with a non-empty queue", async () => {
    setOnline(true);
    countQueue.mockResolvedValue(3);
    render(<OfflineIndicator />);

    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent(/syncing 3 changes/i),
    );
    expect(screen.getByRole("status")).toHaveAttribute("data-online", "true");
  });

  it("re-counts when the queue subscription fires", async () => {
    setOnline(false);
    render(<OfflineIndicator />);
    await act(async () => {});
    expect(screen.getByRole("status")).toHaveTextContent(
      /changes will sync when you reconnect/i,
    );

    // A queue write happens — the subscription callback fires and the
    // component re-counts.
    countQueue.mockResolvedValue(5);
    await act(async () => {
      queueListener?.();
    });
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent(/5 changes queued/i),
    );
  });

  it("reacts to the browser offline/online events", async () => {
    render(<OfflineIndicator />);
    await act(async () => {});
    // Online + empty queue → nothing rendered.
    expect(screen.queryByRole("status")).toBeNull();

    setOnline(false);
    await act(async () => {
      window.dispatchEvent(new Event("offline"));
    });
    expect(screen.getByRole("status")).toHaveTextContent(/you're offline/i);

    setOnline(true);
    await act(async () => {
      window.dispatchEvent(new Event("online"));
    });
    await waitFor(() => expect(screen.queryByRole("status")).toBeNull());
  });
});
