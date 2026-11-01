# arco-nbio

Arco Cloud's fork of [lesismal/nbio](https://github.com/lesismal/nbio) with its non-blocking TLS rebuilt on the
**current Go `crypto/tls` (go1.27.3)** instead of `lesismal/llib`'s 2021 copy. Experimental candidate for
`arco-ws`'s TLS engine; see the verdict before adopting it.

- [docs/design.md](docs/design.md) — what changed versus upstream nbio and versus Go `crypto/tls`, and how the
  non-blocking layer works.
- [docs/porting.md](docs/porting.md) — procedure and checklist for each Go security release.
- [docs/results.md](docs/results.md) — tests, fuzzing, govulncheck, memory and latency; the verdict.

Use from another module:

```
require github.com/lesismal/nbio v1.7.1
replace github.com/lesismal/nbio => ../arco-nbio
```

and import `github.com/lesismal/nbio/crypto/tls` wherever llib's `std/crypto/tls` was used (`nbhttp.Config.TLSConfig`
takes a `*tls.Config` from that package). Build with go1.27.3 or a later 1.27.x.

Branch `arco` holds the fork; `upstream` is lesismal/nbio. Nothing has been pushed anywhere.
