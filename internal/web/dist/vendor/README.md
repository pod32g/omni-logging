# Vendored third-party assets

Committed rather than fetched at build time, because the UI is embedded into the
binary with `go:embed` and must work air-gapped — nothing here may be loaded
from a CDN.

| File | Upstream | Version | License |
|---|---|---|---|
| `uPlot.min.js` | [leeoniya/uPlot](https://github.com/leeoniya/uPlot) `dist/uPlot.iife.min.js` | 1.6.32 | MIT (`uPlot.LICENSE`) |
| `uPlot.min.css` | same, `dist/uPlot.min.css` | 1.6.32 | MIT |

uPlot has no dependencies of its own. To update: `npm pack uplot`, unpack, copy
`dist/uPlot.iife.min.js` → `uPlot.min.js` and `dist/uPlot.min.css`, refresh the
LICENSE and the version above.
