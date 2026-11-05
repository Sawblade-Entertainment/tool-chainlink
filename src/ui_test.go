package main

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uiScriptPrompt makes confirm read answer, as if typed at a terminal.
func uiScriptPrompt(t *testing.T, interactive bool, answer string) {
	t.Helper()
	oldIn, oldInteractive := promptInput, promptIsInteractive
	promptInput = bufio.NewReader(strings.NewReader(answer))
	promptIsInteractive = func() bool { return interactive }
	t.Cleanup(func() { promptInput, promptIsInteractive = oldIn, oldInteractive })
}

func TestPaint(t *testing.T) {
	if got := paint(false, "hi", sgrBold); got != "hi" {
		t.Errorf("disabled paint = %q", got)
	}
	if got := paint(true, "hi", sgrBold, sgrRed); got != "\x1b[1;31mhi\x1b[0m" {
		t.Errorf("paint = %q", got)
	}
	if got := paint(true, "", sgrBold); got != "" {
		t.Errorf("empty paint = %q", got)
	}
	if got := paint(true, "hi"); got != "hi" {
		t.Errorf("no-code paint = %q", got)
	}
}

func TestStatusVerbAligned(t *testing.T) {
	for _, verb := range []string{"Cached", "Downloading", "Installed"} {
		if got := statusVerb(false, verb, sgrGreen); len(got) != statusVerbWidth || !strings.HasSuffix(got, verb) {
			t.Errorf("statusVerb(%q) = %q", verb, got)
		}
	}
}

func TestDetectColorEnv(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "")
	if detectColor(f) {
		t.Error("a regular file should not get color")
	}
	t.Setenv("FORCE_COLOR", "1")
	if !detectColor(f) {
		t.Error("FORCE_COLOR should enable color")
	}
	t.Setenv("NO_COLOR", "1")
	if detectColor(f) {
		t.Error("NO_COLOR should win over FORCE_COLOR")
	}
}

func TestConfirm(t *testing.T) {
	tests := []struct {
		answer string
		want   bool
	}{
		{"\n", true},
		{"y\n", true},
		{"YES\n", true},
		{"  y  \r\n", true},
		{"n\n", false},
		{"no\n", false},
		{"maybe\n", false},
		{"y", true}, // EOF after an answer still counts
		{"", false}, // EOF with no answer declines
	}
	for _, tt := range tests {
		uiScriptPrompt(t, true, tt.answer)
		got, err := confirm("Proceed?")
		if err != nil || got != tt.want {
			t.Errorf("confirm with %q = %v, %v; want %v", tt.answer, got, err, tt.want)
		}
	}
}

func TestConfirmNotInteractive(t *testing.T) {
	uiScriptPrompt(t, false, "y\n")
	if ok, err := confirm("Proceed?"); ok || !errors.Is(err, errNotInteractive) {
		t.Fatalf("confirm = %v, %v", ok, err)
	}
}

func TestParseInstallArgs(t *testing.T) {
	tests := []struct {
		args    []string
		yes     bool
		wantErr bool
	}{
		{nil, false, false},
		{[]string{"-y"}, true, false},
		{[]string{"--yes"}, true, false},
		{[]string{"-y", "-y"}, true, false},
		{[]string{"--force"}, false, true},
		{[]string{"selene"}, false, true},
	}
	for _, tt := range tests {
		yes, err := parseInstallArgs(tt.args)
		if yes != tt.yes || (err != nil) != tt.wantErr {
			t.Errorf("parseInstallArgs(%q) = %v, %v", tt.args, yes, err)
		}
	}
}

func TestPlanDownloads(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(packageInstallDir(home, "sleitnick", "knit", "1.7.0"), 0755)
	os.MkdirAll(filepath.Join(home, ".chainlink", "tools", "selene", "v0.32.0"), 0755)

	config := ChainLinkConfig{
		Packages: map[string]string{
			"Knit":   "sleitnick/knit@1.7.0", // cached
			"Signal": "sleitnick/signal@2.0.0",
			"Charm":  "littensy/charm@0.9.0",
		},
		Tools: map[string]string{
			"selene": "Kampfkarren/selene@0.32.0", // cached under the v-prefixed tag
			"stylua": "JohnnyMorganz/StyLua@2.0.0",
		},
	}
	pending, err := planDownloads(config, home)
	if err != nil {
		t.Fatal(err)
	}
	want := []pendingDownload{
		{"package", "littensy/charm@0.9.0", "wally.run"},
		{"package", "sleitnick/signal@2.0.0", "wally.run"},
		{"tool", "JohnnyMorganz/StyLua@2.0.0", "github.com/JohnnyMorganz/StyLua"},
	}
	if len(pending) != len(want) {
		t.Fatalf("pending = %+v", pending)
	}
	for i := range want {
		if pending[i] != want[i] {
			t.Errorf("pending[%d] = %+v, want %+v", i, pending[i], want[i])
		}
	}

	config.Tools["bad"] = "not-a-spec"
	if _, err := planDownloads(config, home); err == nil || !strings.Contains(err.Error(), "tool bad") {
		t.Errorf("bad spec err = %v", err)
	}
}

func TestConfirmDownloads(t *testing.T) {
	pending := []pendingDownload{{"package", "a/b@1.0.0", "wally.run"}}

	uiScriptPrompt(t, false, "")
	if err := confirmDownloads(nil, false); err != nil {
		t.Errorf("nothing to download should not prompt: %v", err)
	}
	if err := confirmDownloads(pending, true); err != nil {
		t.Errorf("-y should skip the prompt: %v", err)
	}
	err := confirmDownloads(pending, false)
	if !errors.Is(err, errNotInteractive) || !strings.Contains(err.Error(), "install -y") {
		t.Errorf("non-interactive err = %v", err)
	}

	uiScriptPrompt(t, true, "n\n")
	if err := confirmDownloads(pending, false); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("declined err = %v", err)
	}
	uiScriptPrompt(t, true, "\n")
	if err := confirmDownloads(pending, false); err != nil {
		t.Errorf("accepted err = %v", err)
	}
}

// uiInstallProject writes a chain.toml with one package and a fake registry for it.
func uiInstallProject(t *testing.T) (home string, w *installFakePackageRegistry) {
	t.Helper()
	home = cliSetHome(t)
	cliChdirTemp(t)
	cliWriteFile(t, "chain.toml", "[Packages]\nKnit = \"sleitnick/knit@1.7.0\"\n", 0644)
	w = installNewFakePackageRegistry(t)
	w.pkgs["sleitnick/knit/1.7.0"] = installMakeZip(t, []installEntry{{Name: "src/init.luau", Body: "return {}"}})
	return home, w
}

func TestInstallAllDeclinedDownloadsNothing(t *testing.T) {
	_, w := uiInstallProject(t)
	uiScriptPrompt(t, true, "n\n")
	if err := installAll(false); err == nil {
		t.Fatal("expected cancel error")
	}
	if n := w.requests.Load(); n != 0 {
		t.Errorf("made %d requests after the user declined", n)
	}
}

func TestInstallAllNonInteractiveNeedsYes(t *testing.T) {
	home, w := uiInstallProject(t)
	uiScriptPrompt(t, false, "")
	if err := installAll(false); !errors.Is(err, errNotInteractive) {
		t.Fatalf("err = %v", err)
	}
	if n := w.requests.Load(); n != 0 {
		t.Errorf("made %d requests without confirmation", n)
	}

	if err := installAll(true); err != nil {
		t.Fatalf("install -y: %v", err)
	}
	if !dirExists(packageInstallDir(home, "sleitnick", "knit", "1.7.0")) {
		t.Error("package not installed with -y")
	}

	// Everything is cached now, so a plain install needs no confirmation.
	if err := installAll(false); err != nil {
		t.Errorf("cached install err = %v", err)
	}
}

func TestInstallAllAccepted(t *testing.T) {
	home, _ := uiInstallProject(t)
	uiScriptPrompt(t, true, "y\n")
	if err := installAll(false); err != nil {
		t.Fatal(err)
	}
	if !dirExists(packageInstallDir(home, "sleitnick", "knit", "1.7.0")) {
		t.Error("package not installed after confirming")
	}
}

// Regression: in tool-proxy mode, install progress went to stdout and mixed
// with the tool's own output.
func TestStatusWritesToUIOut(t *testing.T) {
	var buf strings.Builder
	old := uiOut
	uiOut = &buf
	t.Cleanup(func() { uiOut = old })

	status("Downloading", sgrCyan, "%s", "a/b@1.0.0")
	hintf("note")
	if got := buf.String(); !strings.Contains(got, "Downloading a/b@1.0.0\n") || !strings.Contains(got, "note\n") {
		t.Errorf("uiOut = %q", got)
	}
}

// Regression: each confirm used a new bufio.Reader, dropping answers that
// were typed ahead of a later prompt.
func TestConfirmKeepsTypedAhead(t *testing.T) {
	uiScriptPrompt(t, true, "y\nn\n")
	first, _ := confirm("First?")
	second, err := confirm("Second?")
	if !first || second || err != nil {
		t.Errorf("answers = %v, %v, %v; want true, false, nil", first, second, err)
	}
}
