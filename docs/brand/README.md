# Parallax brand assets

The icon uses two interlocking rotated squares in a purple-to-cyan gradient,
with endpoint dots on a dark background. The reusable assets are listed below.

| File | Size | Use |
|---|---|---|
| `parallax-appicon.png` | 512 x 512 | The icon as drawn, transparent squircle corners |
| `parallax-mark.png` | 512 x 512 | Same artwork, for embedding |
| `parallax-mono.png` | 512 x 512 | White mark on transparency, for single-colour contexts |
| `parallax-lockup.jpg` | 1400 x 420 | Icon + wordmark, for READMEs and slides |
| `parallax-banner.jpg` | 1280 x 640 | GitHub social preview (Settings -> Social preview) |

The app-served derivatives are in `frontend/public/` and wired up in `frontend/index.html`:
`favicon.ico`, `favicon-16.png`, `favicon-32.png`, `apple-touch-icon.png` (180), `icon-192.png`,
`icon-512.png`, `og-banner.jpg` (1200 x 630) and `manifest.json`.

## Two things about this set are deliberate

**The 16px favicon is a different drawing.** Two overlapping thin-stroke outlines cannot resolve in
16 pixels — at that size the full artwork turns into a coloured smudge. So `favicon-16.png`, and the
16x16 entry inside `favicon.ico`, use a reduced mark: one diamond outline plus the two endpoint dots,
in the same gradient. Every size from 32px up is the artwork as drawn. `favicon.ico` therefore
carries **per-size artwork** rather than one image resampled, which is what a multi-resolution ICO is
for — note that PIL's ICO writer resamples a single source and cannot produce this.

**The lockup and the banner are composed, not drawn.** The icon has no wordmark, so
those two set the icon beside "Parallax" on the icon's own background colour. If a real wordmark is
ever commissioned, replace them rather than editing them.

`apple-touch-icon.png` is deliberately **opaque** (corners filled with the tile colour) because iOS
applies its own mask and does not handle transparency here; the PWA icons and favicons keep the
transparent squircle corners.

## Colours

Sampled from the artwork:

| Role | Hex | Note |
|---|---|---|
| Tile background | `#17171c` | The squircle |
| Gradient start | `#8561f8` | Purple, upper-left of the mark |
| Gradient end | `#09b3d1` | Cyan, lower-right |
| App background | `#121218` | `--color-bg-primary`, and the `theme-color` meta |

## Regenerating

The icon's dark tile fills the full canvas; only the squircle corners are white, so the alpha mask
comes from luminance (white -> transparent). The favicons crop tighter than the tile — the artwork
carries a wide margin, and reclaiming it is what makes 32px legible. Downscale with Lanczos and add a
light unsharp mask (radius 0.6, 80%, threshold 2) at 48px and below.
