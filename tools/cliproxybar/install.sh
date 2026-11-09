#!/bin/zsh
# Build the CliproxyBar menu bar app (this directory), install it to ~/Applications and start it
# at login via LaunchAgent local.cliproxybar. Re-run after changing the app.
set -euo pipefail

SRC=${0:A:h}
APP=~/Applications/CliproxyBar.app
LABEL=local.cliproxybar
PLIST=~/Library/LaunchAgents/$LABEL.plist

cd $SRC
swift test
swift build -c release

mkdir -p $APP/Contents/MacOS
cp .build/release/CliproxyBar $APP/Contents/MacOS/CliproxyBar
cat > $APP/Contents/Info.plist <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleIdentifier</key><string>local.cliproxybar</string>
  <key>CFBundleName</key><string>CliproxyBar</string>
  <key>CFBundleExecutable</key><string>CliproxyBar</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>1.0</string>
  <key>LSMinimumSystemVersion</key><string>14.0</string>
  <key>LSUIElement</key><true/>
</dict></plist>
EOF
codesign --force --sign - $APP

cat > $PLIST <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key><array><string>$APP/Contents/MacOS/CliproxyBar</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/cliproxybar.log</string>
</dict></plist>
EOF

launchctl bootout gui/$(id -u)/$LABEL 2>/dev/null || true
for _ in {1..50}; do launchctl print gui/$(id -u)/$LABEL >/dev/null 2>&1 || break; sleep 0.1; done
launchctl bootstrap gui/$(id -u) $PLIST
echo "CliproxyBar installed and running ($APP)."
