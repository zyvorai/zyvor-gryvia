# Social cards

Gryvia's share card for GitHub's link previews, the Docusaurus site's Open Graph image
(`website/static/img/gryvia-share-card.png`, kept byte-identical to the light card), and any other
place a hero image is needed.

## Palette

Apple-style light/dark, matching the rest of the Zyvor project family:

| | Light | Dark |
|---|---|---|
| Background | `#ffffff` → `#f5f5f7` | `#000000` → `#0b0b0f` |
| Ink (text) | `#1d1d1f` | `#f5f5f7` |
| Secondary text | `#6e6e73` | `#a1a1a6` |
| Card | `#ffffff` / `#d2d2d7` border | `#1c1c1e` / `#3a3a3c` border |
| Blue accent | `#0071e3` → `#2997ff` | `#0a84ff` → `#5eb0ff` |
| Orange accent | `#ff6a2a` (exactly one dot per image, on the flagship "Cluster A / H100" node) | same |

Fonts: Helvetica Neue (headings/body), Menlo (labels, pills, footer path). The Zyvor "Z" mark is
drawn inline in blue — never the orange brand tile — matching fluxvm/fabric/atlas/etc.

## Files

- `build-share-cards.py` — generates the SVG sources from one palette dict.
- `gryvia-share-card.svg` / `.png` — 1200×630, light. This is the GitHub Social Preview image and the
  Docusaurus site's `themeConfig.image`.
- `gryvia-share-card-dark.svg` / `.png` — 1200×630, dark. Not currently referenced anywhere
  automatically (GitHub Social Preview and Docusaurus `themeConfig.image` are both single, static
  images); kept for anywhere a dark-mode hero is wired up later (e.g. a README `<picture>` element).

## Rebuild

```bash
python3 docs/social/build-share-cards.py docs/social
rsvg-convert -w 1200 docs/social/gryvia-share-card.svg      -o docs/social/gryvia-share-card.png
rsvg-convert -w 1200 docs/social/gryvia-share-card-dark.svg -o docs/social/gryvia-share-card-dark.png
cp docs/social/gryvia-share-card.png website/static/img/gryvia-share-card.png
```

`rsvg-convert` ships with `librsvg` (`brew install librsvg` on macOS).

## Honesty note

Gryvia is early stage: the README says "Performance figures in this repository are design targets,
not measured results." The card itself carries an "EARLY STAGE · DESIGN TARGETS, NOT MEASURED" note
for the same reason the README does — it gets shared and viewed standalone (link previews, socials)
without the surrounding disclaimer. Do not remove that line, and do not add numeric performance claims
to the card.

## Manual step: GitHub Social Preview

GitHub's repository Social Preview (shown when the repo link is shared) is **not** settable via the
API — upload `docs/social/gryvia-share-card.png` by hand:

Settings → General → Social preview → Edit → upload `docs/social/gryvia-share-card.png`.
