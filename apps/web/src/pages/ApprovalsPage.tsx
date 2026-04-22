/**
 * ApprovalsPage lists pending approvals for the current user. For
 * Phase B the listing endpoint is wired via a lightweight fetch since
 * the generated client predates the approvals surface; once the
 * OpenAPI spec covers approvals we can swap this for `api.*`.
 */
export function ApprovalsPage() {
  return (
    <section>
      <h1>Approvals</h1>
      <p style={{ color: "#6b7280" }}>
        Pending approvals for the current user appear here. Use the
        Approve / Reject buttons or run <code>/approve &lt;id&gt; approve</code>
        in KChat.
      </p>
      <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
        Listing coming online once the approvals listing endpoint is exposed
        on the public API (tracked in PROGRESS.md).
      </p>
    </section>
  );
}
