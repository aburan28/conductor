# Conductor.app icon

The Dock / Finder / About mark matches the web dashboard favicon: a green
rounded square (`#2f6f4e`) with three white horizontal strokes.

| File | Role |
|---|---|
| `AppIcon.svg` | Source on Apple's 1024pt icon grid |
| `favicon.svg` | Compact copy of the HTML favicon |
| `AppIcon-1024.png` | Preview / App Store–style master |
| `AppIcon.icns` | What `macos/build.sh` copies into `Contents/Resources` |
| `make-icon.py` | Regenerates the PNG and `.icns` from the mark |

```sh
python3 -m pip install pillow icnsutil
python3 macos/packaging/icon/make-icon.py
```

`Info.plist` sets `CFBundleIconFile` to `AppIcon`. The menu bar draws the same
mark in code (`StatusDotImage`) tinted by status.
