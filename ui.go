package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ANSI SGR codes used by the CLI.
const (
	sgrBold   = "1"
	sgrDim    = "2"
	sgrRed    = "31"
	sgrGreen  = "32"
	sgrYellow = "33"
	sgrBlue   = "34"
	sgrCyan   = "36"
)

// uiOut is where status lines and prompts go: stdout, except in tool-proxy
// mode, where stdout belongs to the tool. colorOut and colorStderr say whether
// uiOut and stderr get ANSI styling; tests leave them false so output stays plain.
var (
	uiOut       io.Writer = os.Stdout
	colorOut              = detectColor(os.Stdout)
	colorStderr           = detectColor(os.Stderr)
)

// detectColor follows the NO_COLOR / FORCE_COLOR conventions, and otherwise
// colors only real terminals (enabling VT processing on Windows consoles).
func detectColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if v := os.Getenv("FORCE_COLOR"); v != "" && v != "0" {
		return true
	}
	if os.Getenv("TERM") == "dumb" || !term.IsTerminal(int(f.Fd())) {
		return false
	}
	return enableVirtualTerminal(f)
}

// paint wraps s in the given SGR codes when enabled.
func paint(enabled bool, s string, codes ...string) string {
	if !enabled || len(codes) == 0 || s == "" {
		return s
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + s + "\x1b[0m"
}

// Styling shorthands for text written to uiOut.
func bold(s string) string  { return paint(colorOut, s, sgrBold) }
func dim(s string) string   { return paint(colorOut, s, sgrDim) }
func cyan(s string) string  { return paint(colorOut, s, sgrCyan) }
func green(s string) string { return paint(colorOut, s, sgrGreen) }

// statusVerbWidth right-aligns status verbs so messages line up, cargo-style.
const statusVerbWidth = 12

// status prints "   Downloading selene 0.32.0" with the verb bold and colored.
func status(verb, color, format string, args ...any) {
	fmt.Fprintf(uiOut, "%s %s\n", statusVerb(colorOut, verb, color), fmt.Sprintf(format, args...))
}

func statusVerb(enabled bool, verb, color string) string {
	return paint(enabled, fmt.Sprintf("%*s", statusVerbWidth, verb), sgrBold, color)
}

// warnf prints a yellow "warning:" line to stderr.
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", paint(colorStderr, "warning:", sgrBold, sgrYellow), fmt.Sprintf(format, args...))
}

// hintf prints a dimmed follow-up line, indented under status messages.
func hintf(format string, args ...any) {
	fmt.Fprintf(uiOut, "%*s %s\n", statusVerbWidth, "", dim(fmt.Sprintf(format, args...)))
}

// fail prints a red "error:" line to stderr and exits with status 1.
func fail(context string, err error) {
	msg := err.Error()
	if context != "" {
		msg = context + ": " + msg
	}
	fmt.Fprintf(os.Stderr, "%s %s\n", paint(colorStderr, "error:", sgrBold, sgrRed), msg)
	os.Exit(1)
}

// usageFail prints the correct usage for a command and exits with status 2.
func usageFail(usage string) {
	fmt.Fprintf(os.Stderr, "%s %s\n", paint(colorStderr, "usage:", sgrBold, sgrYellow), usage)
	os.Exit(2)
}

// promptInput and promptIsInteractive are variables so tests can script
// answers. promptInput is one shared reader so input typed ahead of a later
// prompt isn't lost in a discarded buffer.
var (
	promptInput         = bufio.NewReader(os.Stdin)
	promptIsInteractive = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
)

// errNotInteractive is returned by confirm when nobody can answer the prompt.
var errNotInteractive = errors.New("confirmation required but stdin is not a terminal")

// confirm asks a yes/no question; an empty answer means yes.
func confirm(question string) (bool, error) {
	if !promptIsInteractive() {
		return false, errNotInteractive
	}
	fmt.Fprintf(uiOut, "%s %s %s ", paint(colorOut, "?", sgrBold, sgrCyan), bold(question), dim("[Y/n]"))
	line, err := promptInput.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		fmt.Fprintln(uiOut)
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true, nil
	}
	return false, nil
}

// logTag renders a live-sync log tag such as "[CREATE]" for stderr.
func logTag(op string) string {
	color := sgrCyan
	switch op {
	case "CREATE", "PKG ADD", "PROMOTE":
		color = sgrGreen
	case "UPDATE", "BOOTSTRAP":
		color = sgrBlue
	case "DELETE", "PKG REMOVE", "UNMERGE":
		color = sgrRed
	}
	return paint(colorStderr, "["+op+"]", sgrBold, color)
}

// logWarn is the "Warning:" prefix for live-sync log lines.
func logWarn() string { return paint(colorStderr, "Warning:", sgrBold, sgrYellow) }
