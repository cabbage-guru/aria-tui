package hotkey

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const serviceName = "Aria TUI - Queue Download"

func serviceDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Services")
}

func workflowPath() string {
	return filepath.Join(serviceDir(), serviceName+".workflow")
}

// IsInstalled returns true if the Quick Action is already installed.
func IsInstalled() bool {
	_, err := os.Stat(workflowPath())
	return err == nil
}

// Install creates a macOS Automator Quick Action (.workflow) in ~/Library/Services
// that runs 'aria-tui clip'. The user can then assign a global keyboard shortcut
// via System Settings > Keyboard > Keyboard Shortcuts > Services.
func Install() error {
	binaryPath, err := os.Executable()
	if err != nil {
		binaryPath, err = exec.LookPath("aria-tui")
		if err != nil {
			return fmt.Errorf("cannot find aria-tui binary: %w", err)
		}
	}
	// Resolve symlinks so the absolute path survives across environments
	binaryPath, _ = filepath.EvalSymlinks(binaryPath)

	wfDir := workflowPath()
	contentsDir := filepath.Join(wfDir, "Contents")
	if err := os.MkdirAll(contentsDir, 0755); err != nil {
		return fmt.Errorf("creating workflow directory: %w", err)
	}

	// Write Info.plist
	infoPlist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>NSServices</key>
	<array>
		<dict>
			<key>NSMenuItem</key>
			<dict>
				<key>default</key>
				<string>Aria TUI - Queue Download</string>
			</dict>
			<key>NSMessage</key>
			<string>runWorkflowAsService</string>
		</dict>
	</array>
</dict>
</plist>
`
	if err := os.WriteFile(filepath.Join(contentsDir, "Info.plist"), []byte(infoPlist), 0644); err != nil {
		return fmt.Errorf("writing Info.plist: %w", err)
	}

	// Write document.wflow — Automator workflow that runs a shell script
	documentWflow := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>AMApplicationBuild</key>
	<string>523</string>
	<key>AMApplicationVersion</key>
	<string>2.10</string>
	<key>AMDocumentVersion</key>
	<string>2</string>
	<key>actions</key>
	<array>
		<dict>
			<key>action</key>
			<dict>
				<key>AMAccepts</key>
				<dict>
					<key>Container</key>
					<string>List</string>
					<key>Optional</key>
					<true/>
					<key>Types</key>
					<array>
						<string>com.apple.cocoa.string</string>
					</array>
				</dict>
				<key>AMActionVersion</key>
				<string>2.0.3</string>
				<key>AMApplication</key>
				<array>
					<string>Automator</string>
				</array>
				<key>AMBundleIdentifier</key>
				<string>com.apple.RunShellScript</string>
				<key>AMCategory</key>
				<array>
					<string>AMCategoryUtilities</string>
				</array>
				<key>AMIconName</key>
				<string>Automator</string>
				<key>AMKeywords</key>
				<array>
					<string>Shell</string>
					<string>Script</string>
				</array>
				<key>AMName</key>
				<string>Run Shell Script</string>
				<key>AMProvides</key>
				<dict>
					<key>Container</key>
					<string>List</string>
					<key>Types</key>
					<array>
						<string>com.apple.cocoa.string</string>
					</array>
				</dict>
				<key>ActionBundlePath</key>
				<string>/System/Library/Automator/Run Shell Script.action</string>
				<key>ActionName</key>
				<string>Run Shell Script</string>
				<key>ActionParameters</key>
				<dict>
					<key>COMMAND_STRING</key>
					<string>export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
OUTPUT=$(%s clip 2&gt;&amp;1)
if [ $? -eq 0 ]; then
  osascript -e "display notification \"$OUTPUT\" with title \"aria-tui\""
else
  osascript -e "display notification \"$OUTPUT\" with title \"aria-tui\" subtitle \"Error\""
fi</string>
					<key>CheckedForUserDefaultShell</key>
					<true/>
					<key>inputMethod</key>
					<integer>0</integer>
					<key>shell</key>
					<string>/bin/zsh</string>
					<key>source</key>
					<string></string>
				</dict>
				<key>BundleIdentifier</key>
				<string>com.apple.RunShellScript</string>
				<key>CFBundleVersion</key>
				<string>2.0.3</string>
				<key>CanShowSelectedItemsWhenRun</key>
				<false/>
				<key>CanShowWhenRun</key>
				<true/>
				<key>Category</key>
				<array>
					<string>AMCategoryUtilities</string>
				</array>
				<key>Class Name</key>
				<string>RunShellScriptAction</string>
				<key>InputUUID</key>
				<string>A1A1A1A1-B2B2-C3C3-D4D4-E5E5E5E5E5E5</string>
				<key>Keywords</key>
				<array>
					<string>Shell</string>
					<string>Script</string>
				</array>
				<key>OutputUUID</key>
				<string>F6F6F6F6-A7A7-B8B8-C9C9-D0D0D0D0D0D0</string>
				<key>UUID</key>
				<string>11111111-2222-3333-4444-555555555555</string>
				<key>UnlocalizedApplications</key>
				<array>
					<string>Automator</string>
				</array>
			</dict>
		</dict>
	</array>
	<key>connectors</key>
	<dict/>
	<key>workflowMetaData</key>
	<dict>
		<key>serviceInputTypeIdentifier</key>
		<string>com.apple.Automator.nothing</string>
		<key>serviceProcessesInput</key>
		<integer>0</integer>
		<key>workflowTypeIdentifier</key>
		<string>com.apple.Automator.servicesMenu</string>
	</dict>
</dict>
</plist>
`, binaryPath)

	if err := os.WriteFile(filepath.Join(contentsDir, "document.wflow"), []byte(documentWflow), 0644); err != nil {
		return fmt.Errorf("writing document.wflow: %w", err)
	}

	return nil
}

// Uninstall removes the Quick Action.
func Uninstall() error {
	return os.RemoveAll(workflowPath())
}

// Instructions returns the steps to assign a keyboard shortcut after installation.
func Instructions() string {
	return `Quick Action installed! To assign a global keyboard shortcut:

  1. Open System Settings > Keyboard > Keyboard Shortcuts > Services
  2. Scroll to "General" and find "Aria TUI - Queue Download"
  3. Click "none" next to it and press your desired shortcut
     (e.g. Cmd+Shift+Ctrl+D)

You'll see a macOS notification on success/failure. The TUI must be running.`
}
