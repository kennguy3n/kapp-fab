# Records list pagination

The `GET /api/v1/records/{ktype}` endpoint supports two pagination modes:

1. **Cursor (recommended).** Server returns an opaque `next_cursor` and the
   client passes it back on the next request. Stable under concurrent
   inserts because the server uses keyset pagination
   (`WHERE (updated_at, id) < (cursor_ts, cursor_id)`).
2. **Offset (deprecated).** Legacy `?offset=N` query param. Still works
   but every response carries `Deprecation: true` and a `Sunset` hint.
   Will be removed in a future major version.

## Request

| Param | Type | Required | Description |
| --- | --- | --- | --- |
| `limit` | int | no | Max records per page. Default 50, cap 500. |
| `cursor` | string | no | Opaque token from a prior page's `next_cursor`. Selects keyset pagination *and* the envelope response shape. |
| `paginate` | string | no | Set to `cursor` to opt into the envelope on the first page (no cursor token yet). |
| `offset` | int | no | **Deprecated.** Legacy OFFSET-based pagination. |
| `status` | string | no | Filter by record status. Defaults to `active`. |

## Response

The response shape is opt-in to preserve backward compatibility:

- `?cursor=...` or `?paginate=cursor` → envelope:

  ```json
  {
    "records": [ { "id": "...", "tenant_id": "...", "data": {...}, ... } ],
    "next_cursor": "MTczMzAwMDAwMDAwMHwwMTk1NS4uLg"
  }
  ```

  `next_cursor` is omitted (or empty) when the page is the last one.

- Otherwise → bare array (legacy):

  ```json
  [ { "id": "...", "tenant_id": "...", "data": {...}, ... } ]
  ```

When `?offset=N` is supplied with no cursor, the response also includes:

```
Deprecation: true
Sunset: Wed, 31 Dec 2026 23:59:59 GMT
Link: </docs/API_PAGINATION.md>; rel="deprecation"; type="text/markdown"
```

## Walking every record

```ts
let cursor: string | undefined;
do {
  const url = new URL(`/api/v1/records/${ktype}`, base);
  url.searchParams.set("paginate", "cursor");
  if (cursor) url.searchParams.set("cursor", cursor);
  const page = await fetch(url).then(r => r.json());
  for (const rec of page.records) process(rec);
  cursor = page.next_cursor || undefined;
} while (cursor);
```

```rust
let mut cursor: Option<String> = None;
loop {
    let page = client.list_records_page(ktype, ListFilter {
        cursor: cursor.clone(),
        limit: Some(500),
        ..Default::default()
    }).await?;
    for rec in &page.records { process(rec); }
    match page.next_cursor {
        Some(c) if !c.is_empty() => cursor = Some(c),
        _ => break,
    }
}
```

## Cursor format

`base64url(<unix_nanos>|<uuid>)` — `unix_nanos` is the `updated_at`
timestamp in nanoseconds since epoch, `<uuid>` is the record id. The
format is intentionally opaque: clients must not parse or generate
tokens themselves. The server appends new fields by extending the
delimiter list; old clients keep working because the decoder ignores
trailing segments.

## Server-side notes

- `record.PGStore.List` (legacy `[]KRecord` return) and
  `record.PGStore.ListPage` (cursor-aware envelope) share a single
  query plan — `List` is just a thin wrapper.
- `record.PGStore.ListAll` and `record.PGStore.ListByField` paginate
  internally using the same keyset pattern (500-row chunks) so
  long-running sweepers (payroll, reorder, scheduler) never see
  duplicated rows under concurrent inserts.
- Cursor encoding lives in `internal/record/record.go`
  (`EncodeCursor`, `DecodeCursor`). Decoder rejects malformed tokens
  with `record.ErrInvalidCursor`, which the HTTP handler surfaces as
  HTTP 400.
