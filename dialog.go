package main

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"unicode/utf16"
)

// pickPath opens a native file dialog. kind is "html", "output" or "folder".
// An empty path with a nil error means the user cancelled.
func pickPath(kind string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		return pickPathWindows(kind)
	case "darwin":
		return pickPathMac(kind)
	default:
		return pickPathLinux(kind)
	}
}

func pickPathWindows(kind string) (string, error) {
	var script string
	switch kind {
	case "html":
		script = `
Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$d = New-Object System.Windows.Forms.OpenFileDialog
$d.Title = '选择 HTML 文件'
$d.Filter = 'HTML 文件 (*.html;*.htm)|*.html;*.htm|所有文件 (*.*)|*.*'
$d.CheckFileExists = $true
$d.Multiselect = $false
if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.WriteLine($d.FileName) }
`
	case "output":
		script = `
Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$d = New-Object System.Windows.Forms.SaveFileDialog
$d.Title = '选择导出位置'
$d.Filter = 'MP4 视频 (*.mp4)|*.mp4|所有文件 (*.*)|*.*'
$d.DefaultExt = 'mp4'
$d.AddExtension = $true
$d.OverwritePrompt = $true
if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.WriteLine($d.FileName) }
`
	case "folder":
		script = `
Add-Type -AssemblyName System.Windows.Forms
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = '选择导出文件夹'
$d.ShowNewFolderButton = $true
if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.WriteLine($d.SelectedPath) }
`
	default:
		return "", fmt.Errorf("未知的对话框类型: %s", kind)
	}
	out, err := runPowerShellSTA(script)
	if err != nil {
		return "", err
	}
	return firstNonEmptyLine(out), nil
}

func runPowerShellSTA(script string) (string, error) {
	encoded := encodeUTF16LEBase64(script)
	var lastErr error
	for _, exe := range []string{"powershell.exe", "powershell", "pwsh.exe", "pwsh"} {
		cmd := exec.Command(exe, "-NoProfile", "-NonInteractive", "-STA", "-EncodedCommand", encoded)
		configureChild(cmd)
		out, err := cmd.Output()
		if err == nil {
			return string(out), nil
		}
		lastErr = fmt.Errorf("%s: %w", exe, err)
	}
	return "", fmt.Errorf("无法调用 PowerShell 打开文件对话框: %v", lastErr)
}

func encodeUTF16LEBase64(s string) string {
	u := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(u)*2)
	for _, r := range u {
		buf = append(buf, byte(r), byte(r>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func firstNonEmptyLine(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line != "" {
			return line
		}
	}
	return ""
}

func pickPathMac(kind string) (string, error) {
	var script string
	switch kind {
	case "html":
		script = `POSIX path of (choose file with prompt "选择 HTML 文件" of type {"public.html", "public.xml"})`
	case "output":
		script = `POSIX path of (choose file name with prompt "选择导出位置" default name "chromaframe.mp4")`
	case "folder":
		script = `POSIX path of (choose folder with prompt "选择导出文件夹")`
	default:
		return "", fmt.Errorf("未知的对话框类型: %s", kind)
	}
	cmd := exec.Command("osascript", "-e", script)
	out, err := cmd.Output()
	if err != nil {
		// User cancellation exits non-zero with "User canceled." on stderr.
		return "", nil
	}
	return firstNonEmptyLine(string(out)), nil
}

func pickPathLinux(kind string) (string, error) {
	if _, err := exec.LookPath("zenity"); err != nil {
		return "", fmt.Errorf("没有可用的原生文件对话框，请直接输入路径")
	}
	var args []string
	switch kind {
	case "html":
		args = []string{"--file-selection", "--title=选择 HTML 文件", "--file-filter=HTML 文件 | *.html *.htm"}
	case "output":
		args = []string{"--file-selection", "--save", "--confirm-overwrite", "--title=选择导出位置", "--filename=chromaframe.mp4"}
	case "folder":
		args = []string{"--file-selection", "--directory", "--title=选择导出文件夹"}
	default:
		return "", fmt.Errorf("未知的对话框类型: %s", kind)
	}
	out, err := exec.Command("zenity", args...).Output()
	if err != nil {
		return "", nil
	}
	return firstNonEmptyLine(string(out)), nil
}

// fatalDialog reports a startup failure even before the web UI exists.
func fatalDialog(message string) {
	if runtime.GOOS == "windows" {
		script := `
Add-Type -AssemblyName System.Windows.Forms
[System.Windows.Forms.MessageBox]::Show(` + psQuote(message) + `, 'Chromaframe', 'OK', 'Error') | Out-Null
`
		if _, err := runPowerShellSTA(script); err == nil {
			return
		}
	}
	fmt.Println(message)
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
