# KubeAmber logo

<p>
  <img src="icon/color/kubeamber-icon-color.svg" alt="KubeAmber icon" width="96">
  &nbsp;&nbsp;
  <img src="horizontal/color/kubeamber-horizontal-color.svg" alt="KubeAmber" width="360">
</p>

A shield holding a traffic light whose amber lamp is lit: a change that waits
for a human. Green (allow) and red (deny) are present but dimmed, because amber
is what makes KubeAmber different.

## Files

Every logo comes as SVG (use this wherever you can) and PNG.

| Layout | `color` | `color-dark` | `black` | `white` |
|---|---|---|---|---|
| `icon/` | light backgrounds | dark backgrounds | one-colour, on light | one-colour, on dark |
| `horizontal/` | " | " | " | " |
| `stacked/` | " | " | " | " |

- **`color`** is the main logo, for white or light backgrounds.
- **`color-dark`** is for dark backgrounds. The shield is lighter and gains an
  edge, and "Amber" uses the brighter amber.
- **`black`** and **`white`** are for one-colour use: print, stickers,
  embossing, partner pages with strict colour rules.
- `avatar/` is a square for the GitHub organisation and social profiles.
- `social/` is the 1280 x 640 GitHub social preview.
- `favicon/` has `favicon.svg`, `favicon-32.png` and a 180 px
  `apple-touch-icon.png`.

## Colours

| Use | Hex |
|---|---|
| Shield, and "Kube" on light | `#1C1917` |
| Lit amber lamp, and "Amber" on dark | `#F59E0B` |
| "Amber" on light (4.5:1 on white) | `#B45309` |
| Stop lamp (dimmed to 30%) | `#EF4444` |
| Go lamp (dimmed to 30%) | `#22C55E` |
| Shield on dark, and its edge | `#292524`, `#57534E` |
| "Kube" on dark | `#FAFAF9` |

The wordmark is Space Grotesk Bold
([SIL Open Font License](https://github.com/floriankarsten/space-grotesk)),
converted to outlines, so no font is needed to display it.

## Using it

- Leave clear space around the logo of at least the height of one lamp.
- Smallest sizes: the icon at 16 px, the horizontal logo at 100 px wide.
- Don't recolour the lamps, light a different lamp, stretch, rotate, outline
  or add effects to the logo.
- Don't combine it with the Kubernetes logo, which the Linux Foundation's
  trademark guidelines do not allow in other projects' logos. "KubeAmber" in
  text is fine.

## Regenerating

The icon geometry and colours live in
[`hack/artwork/generate.py`](../hack/artwork/generate.py). Change them there,
not in the SVGs:

```sh
pip install fonttools uharfbuzz
curl -fLO 'https://github.com/google/fonts/raw/main/ofl/spacegrotesk/SpaceGrotesk%5Bwght%5D.ttf'
python hack/artwork/generate.py 'SpaceGrotesk[wght].ttf' artwork
hack/artwork/render.sh artwork      # PNGs and favicons; needs Chrome or Chromium
```
