# Development

This project is not looking for external code contributions at this time.

```
nix develop -c just check        # build, vet, lint, test
nix develop -c just docs         # regenerate docs/reference
nix develop -c just docs-check   # fail on drift
```

`docs/reference/*` is generated; do not edit it by hand.

Chromium and Xvfb are in the dev shell; tests that need them skip under
`-short` or when they are missing. Evals (`tools/eval`) drive the real agent
library and the browser executor against synthetic scenarios and sites, live
or recorded. **No private data goes to any model the harness can call**; see
`tools/eval/README.md`.
