<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Screenshots

Empty deliberately. F-Droid reads whatever PNG/JPEG files sit in this
directory as the store listing's screenshots, so a fake or placeholder image
committed here would ship - worse than having none.

No real screenshots have been captured yet. Capture them by hand, as below.

## How to capture them

The short version:

1. **Check nobody else is using the emulator first**: `ps aux | grep emulator`
   and `ss -ltn | grep 8081` should both come back empty. If not, wait.
2. **Bring up a throwaway server** on port 8081, pointed at a scratch database
   and a couple of real EPUBs - never your own `.env` or database.
3. **Boot the `lexarii` AVD** (Android 15, x86_64):
   `emulator -avd lexarii -no-window -no-audio -no-boot-anim -gpu swiftshader_indirect -no-snapshot-save`,
   then wait for `adb shell getprop sys.boot_completed` to return `1`.
4. **Install the release-shaped build**: `make android-build` (debug is fine
   for screenshots; it looks identical to release, since minification is off)
   then `adb install -r android/app/build/outputs/apk/debug/app-debug.apk`.
5. **Drive it and screenshot**: `adb shell input tap x y` /
   `adb shell input text ...` to sign in and navigate (read the screen first
   with `adb shell uiautomator dump`), then
   `adb exec-out screencap -p > shot.png` at each screen worth showing.
6. **Tear down**: stop the server, `emulator -avd lexarii` process, and drop
   the scratch database.

F-Droid's convention for this directory (`fastlane/metadata/android/<locale>/images/phoneScreenshots/`)
takes any number of ordered images; `1.png`, `2.png`, ... is the usual naming.
A reasonable set for Lexarii: the library grid, a book open in the reader,
the text-selection menu making a highlight, and the offline/no-network shelf.
