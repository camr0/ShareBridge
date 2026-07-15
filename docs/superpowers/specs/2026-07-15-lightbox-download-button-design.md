# Lightbox Download Button Design

## Problem

The ShareBridge lightbox download control is created as a plain button with the
classes `lg-download sharebridge-lightbox-download` and the text `↓`. Global
button styles therefore make it a blue button in the normal toolbar flow, while
LightGallery also adds its `\e0f2` download glyph through `::after`. Because the
button lacks `lg-icon`, that private-use glyph renders with the system font as a
blank or missing character.

## Behavior

- Render the download action as a native LightGallery toolbar icon beside the
  Close control.
- Use LightGallery's existing download glyph and icon font rather than a second
  text arrow or a new custom icon.
- Preserve the current accessible name, title, and click behavior for images and
  videos.

## Implementation

Create the control with `lg-icon lg-download sharebridge-lightbox-download` and
leave its text content empty. The existing LightGallery CSS will then provide
the icon, 50 by 47 pixel toolbar sizing, right alignment, hover color, and focus
style. No vendor CSS or global button styles need modification.

## Verification

Add a gallery-controller regression test that asserts the created button has the
native icon classes and no manual text. Run the full web suite, deploy through
`signaling-server/redeploy.sh`, and inspect the live image and video previews to
confirm the button sits next to Close and exposes only the intended icon.
