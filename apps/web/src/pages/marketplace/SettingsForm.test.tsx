import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  FREEFORM_VALIDITY_KEY,
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

  it("identifies the no-schema editor by a stable validity key (round-4 ANALYSIS_0001)", async () => {
    // Round-4 ANALYSIS_0001: the per-editor validity key lets
    // the parent's settingsInvalidKeys Set disambiguate signals
    // from multiple editors that may be mounted at the same
    // time (B6.2 will wire schemas with multiple object-typed
    // fields, each rendering its own NestedJsonEditor). For
    // FreeformJsonEditor (the no-schema fallback) the key is the
    // stable FREEFORM_VALIDITY_KEY constant — pin both the
    // initial-valid signal and the invalid-on-bad-json
    // transition use this key so a future refactor can't
    // silently fork it.
    const calls: Array<[string, boolean]> = [];
    const onValidityChange = (k: string, v: boolean) => {
      calls.push([k, v]);
    };
    render(
      <SettingsForm
        schema={null}
        value={{}}
        onChange={vi.fn()}
        onValidityChange={onValidityChange}
      />,
    );
    // Type unparseable JSON to force the invalid transition.
    const ta = screen.getByPlaceholderText(
      '{"api_key":"…"}',
    ) as HTMLTextAreaElement;
    await userEvent.type(ta, '{{"unterminated');
    // Every signal must use the freeform sentinel key — no other
    // key namespace should ever fire from this branch of the form.
    expect(calls.length).toBeGreaterThan(0);
    for (const [key] of calls) {
      expect(key).toBe(FREEFORM_VALIDITY_KEY);
    }
    // And the final state is invalid (the last signal flipped to false).
    const last = calls[calls.length - 1];
    expect(last[1]).toBe(false);
  });

  it("per-editor validity signals don't race when multiple object-typed schema fields render (round-4 ANALYSIS_0001)", async () => {
    // Round-4 ANALYSIS_0001: a B6.2-shape schema with multiple
    // object-typed properties spins up multiple NestedJsonEditor
    // instances simultaneously. Each carries its own text
    // buffer + parse-validity state, but the parent funnels all
    // signals through a single onValidityChange callback. The
    // pre-fix signature was (valid: boolean), so editor A's
    // "invalid" signal was overwritten by editor B's later
    // "valid" signal — the parent's bit reflected only the most
    // recent signal rather than the conjunction. Save would
    // then be silently enabled while editor A still held
    // unparseable text.
    //
    // The fix carries a per-editor key on every signal so the
    // parent can maintain a Set<string> of invalid keys. This
    // test pins:
    //   1. Two editors signal independently using distinct keys
    //      (their `id` props: "setting-config_a", "setting-config_b").
    //   2. After corrupting BOTH editors and then recovering
    //      ONE, the recovery signal must NOT lose information
    //      about the still-invalid editor — i.e. the recorded
    //      signals must contain the unrecovered key's "false"
    //      with no overwriting "true" for that same key.
    const schema: SettingsSchema = {
      type: "object",
      properties: {
        config_a: { type: "object", title: "Config A" },
        config_b: { type: "object", title: "Config B" },
      },
    };
    const calls: Array<[string, boolean]> = [];
    const onValidityChange = (k: string, v: boolean) => {
      calls.push([k, v]);
    };
    render(
      <SettingsForm
        schema={schema}
        value={{}}
        onChange={vi.fn()}
        onValidityChange={onValidityChange}
      />,
    );
    // The NestedJsonEditor textareas are addressed by their
    // SettingsField label-id pairing (id="setting-${name}").
    // Find both by their id (the textarea has id=).
    const taA = document.getElementById(
      "setting-config_a",
    ) as HTMLTextAreaElement;
    const taB = document.getElementById(
      "setting-config_b",
    ) as HTMLTextAreaElement;
    expect(taA).toBeTruthy();
    expect(taB).toBeTruthy();
    // Corrupt both editors with unparseable text. Each should
    // fire a signal under its own per-editor key.
    await userEvent.type(taA, '{{"unterminated');
    await userEvent.type(taB, '{{"also unterminated');
    // Both keys must have produced at least one invalid signal.
    expect(calls.some(([k, v]) => k === "setting-config_a" && v === false)).toBe(
      true,
    );
    expect(calls.some(([k, v]) => k === "setting-config_b" && v === false)).toBe(
      true,
    );
    // Recover only editor A.
    await userEvent.clear(taA);
    await userEvent.type(taA, '{{"ok":1}');
    // Recovery must be reported under editor A's key — NOT under
    // editor B's. The pre-fix signature would've signalled a
    // bare `true` and the parent would have stomped on B's
    // still-invalid state.
    const recoveryA = calls.find(
      (c, i) => i > 0 && c[0] === "setting-config_a" && c[1] === true,
    );
    expect(recoveryA).toBeDefined();
    // Critical assertion: editor B never silently flipped to
    // valid in the recorded signals. The most recent signal for
    // key "setting-config_b" must still be false.
    const lastB = [...calls]
      .reverse()
      .find(([k]) => k === "setting-config_b");
    expect(lastB).toBeDefined();
    expect(lastB![1]).toBe(false);
  });

  it("unmount cleanup uses the LATEST onValidityChange identity, not the initial-mount one (round-4 ANALYSIS_0004)", async () => {
    // Round-4 ANALYSIS_0004: the unmount cleanup useEffect used
    // an empty dep array, so it captured only the
    // onValidityChange identity from initial render. If the
    // parent passed a fresh closure on a later render (e.g. an
    // inline arrow function rather than a useCallback-stable
    // one), the cleanup path would call the stale initial-mount
    // closure rather than the current one — silently
    // re-validating a parent that no longer exists, or missing
    // the parent that's currently mounted.
    //
    // The fix captures onValidityChange in a useRef updated on
    // every render. The cleanup dereferences the ref at unmount
    // time, so it always reaches the latest callback.
    //
    // We pin the contract by:
    //   1. Mounting an editor with callback cbA, typing
    //      unparseable text to put it into the invalid state.
    //   2. Re-rendering with callback cbB.
    //   3. Unmounting the editor.
    //   4. Asserting cbB (NOT cbA) received the on-unmount
    //      "restore-to-valid" signal.
    const cbA = vi.fn();
    const cbB = vi.fn();
    const { rerender, unmount } = render(
      <SettingsForm
        schema={null}
        value={{}}
        onChange={vi.fn()}
        onValidityChange={cbA}
      />,
    );
    const ta = screen.getByPlaceholderText(
      '{"api_key":"…"}',
    ) as HTMLTextAreaElement;
    // Put the editor into invalid state so the unmount-path
    // restore-to-valid signal will actually fire.
    await userEvent.type(ta, '{{"unterminated');
    // Drain cbA so we can specifically observe cbB's calls.
    expect(cbA).toHaveBeenCalled();
    const callsBeforeRerender = cbA.mock.calls.length;
    // Re-render with a new callback identity. The editor must
    // NOT remount — same React tree, just a prop swap.
    rerender(
      <SettingsForm
        schema={null}
        value={{}}
        onChange={vi.fn()}
        onValidityChange={cbB}
      />,
    );
    // Unmount. The cleanup must signal restore-to-valid via the
    // LATEST callback (cbB) — the pre-fix code captured cbA on
    // mount and would call it here.
    unmount();
    // cbA should NOT have received any further calls after the
    // re-render. The pre-fix path would have routed the unmount
    // restore-to-valid signal through cbA, failing this assertion.
    expect(cbA.mock.calls.length).toBe(callsBeforeRerender);
    // cbB must have received the unmount restore-to-valid signal
    // under the freeform sentinel key.
    expect(cbB).toHaveBeenCalled();
    const cbBLastCall = cbB.mock.calls[cbB.mock.calls.length - 1] as [
      string,
      boolean,
    ];
    expect(cbBLastCall[0]).toBe(FREEFORM_VALIDITY_KEY);
    expect(cbBLastCall[1]).toBe(true);
  });

  it("NestedJsonEditor rejects non-object JSON (number, array, primitive) for object-typed schema fields (round-6 ANALYSIS_0005)", async () => {
    // Round-6 ANALYSIS_0005: NestedJsonEditor renders the
    // free-form JSON textarea for any SCHEMA property declared
    // `type: "object"`. Pre-fix, its onChange parser accepted
    // ANY valid JSON: typing `42` or `[1,2]` would parse cleanly
    // and forward the result to the parent, where the engine's
    // gojsonschema check would 400 on save — confusing UX (the
    // textarea showed no error, Save was enabled, but the
    // server rejected it). The architecturally correct fix is
    // to reject the type at the editor boundary.
    //
    // We pin three observable behaviours:
    //   1. Typing a number (42) surfaces "Expected an object
    //      (got number)" and suppresses onChange.
    //   2. Typing an array ([1,2]) surfaces "Expected an object
    //      (got array)" — the diagnostic explicitly names array
    //      because the user can see brackets on screen and the
    //      "did you mean braces?" cue is immediate.
    //   3. Recovery to a valid object ({"k":1}) clears the error
    //      and fires onChange with the parsed object.
    const schema: SettingsSchema = {
      type: "object",
      properties: {
        config: { type: "object", title: "Config" },
      },
    };
    const onChange = vi.fn();
    const calls: Array<[string, boolean]> = [];
    render(
      <SettingsForm
        schema={schema}
        value={{}}
        onChange={onChange}
        onValidityChange={(k, v) => calls.push([k, v])}
      />,
    );
    const ta = document.getElementById(
      "setting-config",
    ) as HTMLTextAreaElement;
    expect(ta).toBeTruthy();

    // 1. Number — valid JSON, invalid schema type.
    await userEvent.type(ta, "42");
    expect(
      screen.getByText(/Expected an object \(got number\)/i),
    ).toBeInTheDocument();
    // onChange MUST NOT have fired with `42` as the value.
    expect(
      onChange.mock.calls.some(([next]) =>
        Object.values(next as Record<string, unknown>).some(
          (v) => v === 42,
        ),
      ),
    ).toBe(false);
    // Validity signal went red under the editor's id key.
    expect(
      calls.some(([k, v]) => k === "setting-config" && v === false),
    ).toBe(true);

    // 2. Array — valid JSON, invalid schema type. userEvent
    // interprets `[` and `{` as key-descriptor delimiters, so
    // we route the array case through fireEvent.change to send
    // the literal string in a single value-set rather than a
    // keystroke stream. (Doubling `[[` is the documented
    // userEvent escape but is brittle across versions; the
    // fireEvent route is unambiguous and tests the same code
    // path because the editor's onChange handler is invoked
    // identically.)
    fireEvent.change(ta, { target: { value: "[1,2]" } });
    expect(
      screen.getByText(/Expected an object \(got array\)/i),
    ).toBeInTheDocument();

    // 3. Recovery to a valid object.
    fireEvent.change(ta, { target: { value: '{"k":1}' } });
    // Error text disappears once the object parse succeeds.
    expect(
      screen.queryByText(/Expected an object/i),
    ).not.toBeInTheDocument();
    // onChange fired with the parsed object.
    const lastObjectCall = onChange.mock.calls.find(
      ([next]) =>
        typeof next === "object" &&
        next !== null &&
        typeof (next as Record<string, unknown>).config === "object" &&
        (next as Record<string, Record<string, unknown>>).config.k === 1,
    );
    expect(lastObjectCall).toBeDefined();
    // Final validity signal is back to valid.
    const lastCall = [...calls]
      .reverse()
      .find(([k]) => k === "setting-config");
    expect(lastCall).toBeDefined();
    expect(lastCall![1]).toBe(true);
  });

  it("NestedJsonEditor type-checks the value prop at MOUNT for object-typed schema fields (round-7 ANALYSIS_0003)", async () => {
    // Round-7 ANALYSIS_0003: the type-mismatch check inside
    // the keystroke handler only ran on user input — so if
    // the server returned a non-object value for a
    // `type: "object"` field (e.g. `{"config": null}`,
    // which the Go side can emit since nil maps marshal as
    // JSON null without `omitempty`), the textarea would
    // display `"null"` with no error, the validity signal
    // would stay valid, and Save would be enabled until the
    // user typed a single character. A user who opened the
    // editor and clicked Save would round-trip the wrong-
    // type value back to the server, which would 400. The
    // fix mirrors the keystroke check at the lazy
    // initialiser. We pin the three independent invariants
    // separately: (a) textarea shows the raw value bytes
    // (not collapsed to empty — the user needs to see what
    // the server gave us); (b) error message appears
    // immediately; (c) parent's validity callback fires
    // with `false` at mount, not just after a keystroke.
    const schema: SettingsSchema = {
      type: "object",
      properties: {
        config: { type: "object" },
      },
    };
    const onChange = vi.fn();
    const calls: Array<[string, boolean]> = [];
    const onValidityChange = vi.fn((key: string, valid: boolean) => {
      calls.push([key, valid]);
    });
    render(
      <SettingsForm
        schema={schema}
        value={{ config: null }}
        onChange={onChange}
        onValidityChange={onValidityChange}
      />,
    );
    const ta = screen.getByLabelText(/config/i) as HTMLTextAreaElement;
    // (a) Raw bytes visible.
    expect(ta.value).toBe("null");
    // (b) Diagnostic surfaced immediately.
    expect(
      screen.getByText(/Expected an object \(got null\)/i),
    ).toBeInTheDocument();
    // (c) Parent received an `invalid` signal at mount — not
    // just after a keystroke. The first signal for the
    // config field must be `false`.
    const configSignals = calls.filter(([k]) => k === "setting-config");
    expect(configSignals.length).toBeGreaterThan(0);
    expect(configSignals[0][1]).toBe(false);
    // (d) onChange was NOT called — the bad type didn't leak
    // out to the parent draft.
    expect(onChange).not.toHaveBeenCalled();
  });

  it("NestedJsonEditor type-checks the value prop at MOUNT for ARRAYS (round-7 ANALYSIS_0003)", async () => {
    // Symmetric coverage for the array case. The Go side
    // can't emit an array for a `type: "object"` field today
    // (the handler would have to mistakenly assign a slice),
    // but the editor's contract is "reject non-objects" —
    // testing both null and array pins the symmetry of the
    // mount-time check.
    const schema: SettingsSchema = {
      type: "object",
      properties: { config: { type: "object" } },
    };
    const onChange = vi.fn();
    render(
      <SettingsForm
        schema={schema}
        value={{ config: [1, 2, 3] }}
        onChange={onChange}
      />,
    );
    expect(
      screen.getByText(/Expected an object \(got array\)/i),
    ).toBeInTheDocument();
    expect(onChange).not.toHaveBeenCalled();
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

  it("FreeformJsonEditor distinguishes truly-empty from whitespace-only and refuses to silently emit {} on whitespace input (round-8 ANALYSIS_0005)", async () => {
    // Round-8 ANALYSIS_0005: pre-fix, the FreeformJsonEditor
    // short-circuited `trimmed === "" || trimmed === "{}"` to
    // `onChange({})`. That collapsed two architecturally
    // different cases into the same emit:
    //   * raw === ""        → user cleared the textarea
    //                         intending "leave settings empty"
    //   * raw === "   "     → user has populated settings,
    //                         clears them, then accidentally
    //                         types a few spaces. The textarea
    //                         shows " " while the parent's
    //                         payload silently becomes {} —
    //                         a wipe the user didn't intend.
    // Post-fix:
    //   * raw === ""        → emit {} (UI "leave empty" promise)
    //   * trimmed === ""    → set error, suppress onChange, and
    //                         signal invalid so Save is gated
    //   * trimmed === "{}"  → unchanged: explicit reset to {}
    // Pin all three transitions in a single test so future
    // refactors can't silently collapse them again.
    const onChange = vi.fn();
    const onValidityChange = vi.fn<(key: string, valid: boolean) => void>();
    render(
      <SettingsForm
        schema={null}
        value={{ api_key: "old" }}
        onChange={onChange}
        onValidityChange={onValidityChange}
      />,
    );
    const ta = screen.getByPlaceholderText(
      '{"api_key":"…"}',
    ) as HTMLTextAreaElement;
    // 1. Truly empty: simulate the user clearing the box.
    fireEvent.change(ta, { target: { value: "" } });
    expect(onChange).toHaveBeenLastCalledWith({});
    expect(ta.value).toBe("");
    expect(
      screen.queryByText(/Whitespace-only input is not valid JSON/i),
    ).not.toBeInTheDocument();
    const emptyEmitCount = onChange.mock.calls.length;
    // 2. Whitespace-only: the {} emit MUST NOT fire, error
    // message surfaces, and the validity signal flips to
    // invalid via the FREEFORM key so the parent's
    // settingsFormValid bit drops.
    fireEvent.change(ta, { target: { value: "   " } });
    expect(ta.value).toBe("   ");
    expect(
      screen.getByText(/Whitespace-only input is not valid JSON/i),
    ).toBeInTheDocument();
    expect(onChange.mock.calls.length).toBe(emptyEmitCount);
    const lastFreeformValidity = [...onValidityChange.mock.calls]
      .reverse()
      .find(([k]) => k === FREEFORM_VALIDITY_KEY);
    expect(lastFreeformValidity).toBeDefined();
    expect(lastFreeformValidity![1]).toBe(false);
    // 3. Explicit "{}" reset: error clears, onChange({}) fires,
    // and the validity signal recovers to valid.
    fireEvent.change(ta, { target: { value: "{}" } });
    expect(ta.value).toBe("{}");
    expect(
      screen.queryByText(/Whitespace-only input is not valid JSON/i),
    ).not.toBeInTheDocument();
    expect(onChange).toHaveBeenLastCalledWith({});
    const recoveredFreeformValidity = [...onValidityChange.mock.calls]
      .reverse()
      .find(([k]) => k === FREEFORM_VALIDITY_KEY);
    expect(recoveredFreeformValidity).toBeDefined();
    expect(recoveredFreeformValidity![1]).toBe(true);
  });

  it("NestedJsonEditor distinguishes truly-empty from whitespace-only and refuses to silently unset on whitespace input (round-10 ANALYSIS_0002)", async () => {
    // Round-10 ANALYSIS_0002: pre-fix, the NestedJsonEditor
    // collapsed both `raw === ""` and `raw === "   "` into the
    // same `onChange(undefined)` (unsetting the optional
    // object-typed field). That's wrong by the same UX argument
    // round-8 ANALYSIS_0005 fixed for the FreeformJsonEditor:
    //   * raw === ""        → user cleared the textarea
    //                         intending to unset the field.
    //   * raw === "   "     → user has a populated nested
    //                         object, clears it, then types a
    //                         few spaces. The textarea shows
    //                         whitespace while the parent's
    //                         settings draft silently drops the
    //                         key — an unset the user didn't
    //                         intend.
    // Post-fix:
    //   * raw === ""        → emit undefined (UI promise:
    //                         clears the field).
    //   * trimmed === ""    → set error, suppress onChange, and
    //                         signal invalid under the editor's
    //                         id key so Save is gated.
    //   * trimmed === "{}"  → emit {} (explicit empty object).
    // Pin all three transitions so the parity with
    // FreeformJsonEditor can't silently drift again.
    const schema: SettingsSchema = {
      type: "object",
      properties: {
        config: { type: "object", title: "Config" },
      },
    };
    const onChange = vi.fn();
    const calls: Array<[string, boolean]> = [];
    render(
      <SettingsForm
        schema={schema}
        value={{ config: { k: 1 } }}
        onChange={onChange}
        onValidityChange={(k, v) => calls.push([k, v])}
      />,
    );
    const ta = document.getElementById(
      "setting-config",
    ) as HTMLTextAreaElement;
    expect(ta).toBeTruthy();
    // 1. Truly empty: user clears the textarea → emit a fresh
    // settings object with `config` unset (undefined preserved
    // via the parent's `Object.assign({}, value)` + delete
    // pattern — observable as the `config` key being absent
    // from the most recent emit).
    fireEvent.change(ta, { target: { value: "" } });
    expect(ta.value).toBe("");
    expect(
      screen.queryByText(/Whitespace-only input is not valid JSON/i),
    ).not.toBeInTheDocument();
    const emptyEmit = [...onChange.mock.calls].reverse()[0];
    expect(emptyEmit).toBeDefined();
    expect(
      Object.prototype.hasOwnProperty.call(
        emptyEmit![0] as Record<string, unknown>,
        "config",
      ),
    ).toBe(false);
    const emptyEmitCount = onChange.mock.calls.length;
    // 2. Whitespace-only: the unset emit MUST NOT fire, error
    // message surfaces, and the validity signal flips to
    // invalid under the editor's id key so the parent's
    // settingsFormValid bit drops.
    fireEvent.change(ta, { target: { value: "   " } });
    expect(ta.value).toBe("   ");
    expect(
      screen.getByText(/Whitespace-only input is not valid JSON/i),
    ).toBeInTheDocument();
    expect(onChange.mock.calls.length).toBe(emptyEmitCount);
    const lastValidity = [...calls]
      .reverse()
      .find(([k]) => k === "setting-config");
    expect(lastValidity).toBeDefined();
    expect(lastValidity![1]).toBe(false);
    // 3. Explicit "{}" reset: error clears, onChange fires
    // with `config: {}`, and validity recovers to valid.
    fireEvent.change(ta, { target: { value: "{}" } });
    expect(ta.value).toBe("{}");
    expect(
      screen.queryByText(/Whitespace-only input is not valid JSON/i),
    ).not.toBeInTheDocument();
    const lastObjectEmit = [...onChange.mock.calls].reverse()[0];
    expect(lastObjectEmit).toBeDefined();
    expect(
      (lastObjectEmit![0] as Record<string, Record<string, unknown>>).config,
    ).toEqual({});
    const recoveredValidity = [...calls]
      .reverse()
      .find(([k]) => k === "setting-config");
    expect(recoveredValidity).toBeDefined();
    expect(recoveredValidity![1]).toBe(true);
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
