import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  SettingsForm,
  validateAgainstSchema,
  type SettingsSchema,
} from "./SettingsForm";

describe("SettingsForm", () => {
  it("renders the free-form JSON editor when no schema is provided", () => {
    const onChange = vi.fn();
    render(<SettingsForm schema={null} value={{}} onChange={onChange} />);
    expect(
      screen.getByPlaceholderText('{"api_key":"…"}'),
    ).toBeInTheDocument();
  });

  it("renders typed fields for each schema property", () => {
    const schema: SettingsSchema = {
      type: "object",
      properties: {
        api_key: { type: "string", title: "API Key" },
        max_retries: { type: "integer", default: 3 },
        enabled: { type: "boolean" },
      },
      required: ["api_key"],
    };
    render(
      <SettingsForm schema={schema} value={{}} onChange={vi.fn()} />,
    );
    expect(screen.getByLabelText(/API Key/)).toBeInTheDocument();
    expect(screen.getByLabelText(/max_retries/)).toBeInTheDocument();
    expect(screen.getByLabelText(/enabled/)).toBeInTheDocument();
  });

  it("emits typed values via onChange", async () => {
    const schema: SettingsSchema = {
      type: "object",
      properties: {
        api_key: { type: "string" },
        max_retries: { type: "integer" },
      },
    };
    const onChange = vi.fn();
    render(
      <SettingsForm schema={schema} value={{}} onChange={onChange} />,
    );
    await userEvent.type(screen.getByLabelText(/api_key/), "abc");
    // The last call captures the cumulative value (since the
    // parent isn't actually re-rendered — each keystroke emits a
    // new partial). We assert at least one of the calls saw the
    // string-shaped value.
    expect(onChange).toHaveBeenCalled();
    const lastStringCall = onChange.mock.calls.find(
      ([next]) => typeof (next as Record<string, unknown>).api_key === "string",
    );
    expect(lastStringCall).toBeDefined();
  });
});

describe("validateAgainstSchema", () => {
  it("returns null when the document satisfies a basic schema", () => {
    const schema: SettingsSchema = {
      type: "object",
      properties: {
        api_key: { type: "string", minLength: 4 },
        max_retries: { type: "integer", minimum: 0, maximum: 10 },
      },
      required: ["api_key"],
    };
    expect(
      validateAgainstSchema(schema, { api_key: "abcd", max_retries: 3 }),
    ).toBeNull();
  });

  it("rejects a missing required field", () => {
    const schema: SettingsSchema = {
      type: "object",
      properties: { api_key: { type: "string" } },
      required: ["api_key"],
    };
    expect(validateAgainstSchema(schema, {})).toMatch(/required/);
  });

  it("rejects a string below minLength", () => {
    const schema: SettingsSchema = {
      type: "object",
      properties: { api_key: { type: "string", minLength: 4 } },
      required: ["api_key"],
    };
    expect(
      validateAgainstSchema(schema, { api_key: "abc" }),
    ).toMatch(/at least 4 characters/);
  });

  it("rejects an integer outside the inclusive range", () => {
    const schema: SettingsSchema = {
      type: "object",
      properties: { max_retries: { type: "integer", minimum: 0, maximum: 5 } },
    };
    expect(
      validateAgainstSchema(schema, { max_retries: 7 }),
    ).toMatch(/≤ 5/);
  });

  it("rejects an enum value not in the allowed set", () => {
    const schema: SettingsSchema = {
      type: "object",
      properties: { region: { type: "string", enum: ["us", "eu"] } },
    };
    expect(
      validateAgainstSchema(schema, { region: "ap" }),
    ).toMatch(/one of/);
  });
});
