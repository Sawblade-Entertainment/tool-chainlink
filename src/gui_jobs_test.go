package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestGUIHelperProcess stands in for chainlink when GUI tests run a job.
func TestGUIHelperProcess(t *testing.T) {
	if os.Getenv("CHAINLINK_GUI_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	switch args[0] {
	case "install":
		fmt.Println("line one")
		fmt.Fprintln(os.Stderr, "warning: line two")
		fmt.Println("NO_COLOR=" + os.Getenv("NO_COLOR"))
		os.Exit(0)
	case "assets":
		fmt.Println("boom")
		os.Exit(3)
	case "login-cloud":
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Printf("args=%d key-length=%d\n", len(args), len(strings.TrimSpace(line)))
		os.Exit(0)
	case "serve", "update":
		fmt.Println("started")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(2)
}

func fakeGUIJobs(t *testing.T) *guiJobs {
	t.Helper()
	t.Setenv("CHAINLINK_GUI_HELPER", "1")
	s := newGUIJobs()
	s.command = func(ctx context.Context, args ...string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestGUIHelperProcess$", "--"}, args...)...), nil
	}
	t.Cleanup(func() { s.stopAll(5 * time.Second) })
	return s
}

func waitJob(t *testing.T, j *guiJob) guiJobView {
	t.Helper()
	select {
	case <-j.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("job %s did not finish", j.action)
	}
	return j.view(0, true)
}

func waitForLine(t *testing.T, j *guiJob, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(j.view(0, true).Lines, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s never printed %q: %v", j.action, want, j.view(0, true).Lines)
}

func TestGUIJobCapturesOutput(t *testing.T) {
	s := fakeGUIJobs(t)
	j, err := s.start("install", "")
	if err != nil {
		t.Fatal(err)
	}
	v := waitJob(t, j)
	if v.Running || v.ExitCode != 0 || v.Error != "" {
		t.Fatalf("finished job = %+v", v)
	}
	if v.Command != "chainlink install -y" {
		t.Errorf("command = %q", v.Command)
	}
	for _, want := range []string{"line one", "warning: line two", "NO_COLOR=1"} {
		if !slices.Contains(v.Lines, want) {
			t.Errorf("output %v is missing %q", v.Lines, want)
		}
	}
	if part := j.view(1, true); part.From != 1 || len(part.Lines) != len(v.Lines)-1 || part.Next != len(v.Lines) {
		t.Errorf("view from 1 = %+v", part)
	}
}

func TestGUIJobExitCode(t *testing.T) {
	s := fakeGUIJobs(t)
	j, err := s.start("assets", "")
	if err != nil {
		t.Fatal(err)
	}
	if v := waitJob(t, j); v.ExitCode != 3 || v.Error != "" || v.Stopped {
		t.Fatalf("failed job = %+v", v)
	}
}

func TestGUIJobInputGoesToStdin(t *testing.T) {
	s := fakeGUIJobs(t)
	if _, err := s.start("login-cloud", "  "); err == nil {
		t.Fatal("blank input accepted")
	}
	if _, err := s.start("install", "ignored"); err != nil {
		t.Fatal(err)
	}
	s.stopAll(5 * time.Second)
	j, err := s.start("login-cloud", " secret-key \n")
	if err != nil {
		t.Fatal(err)
	}
	v := waitJob(t, j)
	// One argument (login-cloud): the key came on stdin, not the command line.
	if !slices.Contains(v.Lines, "args=1 key-length=10") {
		t.Fatalf("output = %v", v.Lines)
	}
}

func TestGUIJobsOneAtATimeExceptServe(t *testing.T) {
	s := fakeGUIJobs(t)
	serve, err := s.start("serve", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.start("serve", ""); err == nil {
		t.Fatal("second serve started")
	}
	update, err := s.start("update", "")
	if err != nil {
		t.Fatalf("foreground job blocked by serve: %v", err)
	}
	if _, err := s.start("install", ""); !errors.Is(err, errGUIBusy) {
		t.Fatalf("second foreground job: err = %v, want errGUIBusy", err)
	}

	waitForLine(t, update, "started")
	update.stop()
	if v := waitJob(t, update); !v.Stopped || v.Running || v.Error != "" {
		t.Fatalf("stopped job = %+v", v)
	}
	if _, err := s.start("install", ""); err != nil {
		t.Fatalf("after stop: %v", err)
	}

	views := s.views()
	if len(views) != 3 || views[0].Action != "install" || views[2].Action != "serve" || !views[2].Running {
		t.Fatalf("views = %+v", views)
	}
	serve.stop()
	waitJob(t, serve)
}

func TestGUIJobUnknownAction(t *testing.T) {
	s := fakeGUIJobs(t)
	for _, a := range []string{"", "publish", "remove", "../../bin/sh"} {
		if _, err := s.start(a, ""); !errors.Is(err, errGUIUnknownAction) {
			t.Errorf("%q: err = %v", a, err)
		}
	}
}

func TestGUIJobKeepsLastLines(t *testing.T) {
	j := &guiJob{}
	for i := range guiMaxLines + 10 {
		j.appendLine(fmt.Sprint(i))
	}
	v := j.view(0, true)
	if v.From != 10 || len(v.Lines) != guiMaxLines || v.Lines[0] != "10" || v.Next != guiMaxLines+10 {
		t.Fatalf("from=%d len=%d first=%q next=%d", v.From, len(v.Lines), v.Lines[0], v.Next)
	}
}

func TestGUIJobsTrimFinished(t *testing.T) {
	s := &guiJobs{}
	running := &guiJob{id: 1, running: true}
	s.jobs = append(s.jobs, running)
	for i := 2; i <= guiMaxJobs+5; i++ {
		s.jobs = append(s.jobs, &guiJob{id: i})
	}
	s.trimLocked()
	if len(s.jobs) != guiMaxJobs || s.jobs[0] != running || s.jobs[1].id != 7 {
		t.Fatalf("kept %d jobs, first ids %d %d", len(s.jobs), s.jobs[0].id, s.jobs[1].id)
	}
}

func guiPost(t *testing.T, h http.Handler, path, token, origin, ctype, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Host = "127.0.0.1:1234"
	if token != "" {
		req.Header.Set(guiTokenHeader, token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGUIJobsHTTP(t *testing.T) {
	g, _, _ := guiTestServer(t)
	g.jobs = fakeGUIJobs(t)
	h := g.handler()
	body := `{"action":"install"}`
	const js = "application/json"

	if rec := guiPost(t, h, "/api/jobs", "", "", js, body); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
	if rec := guiPost(t, h, "/api/jobs", g.token, "https://evil.example", js, body); rec.Code != http.StatusForbidden {
		t.Errorf("foreign origin: %d", rec.Code)
	}
	if rec := guiPost(t, h, "/api/jobs", g.token, "", "text/plain", body); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", rec.Code)
	}
	if rec := guiPost(t, h, "/api/jobs", g.token, "", js, `{"action":"rm"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown action: %d", rec.Code)
	}

	rec := guiPost(t, h, "/api/jobs", g.token, "http://127.0.0.1:1234", js, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var started guiJobView
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	waitJob(t, g.jobs.find(started.ID))

	get := guiGet(t, h, "127.0.0.1:1234", fmt.Sprintf("/api/jobs/%d?from=1", started.ID), g.token)
	var v guiJobView
	if err := json.Unmarshal(get.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.From != 1 || len(v.Lines) == 0 || slices.Contains(v.Lines, "line one") {
		t.Errorf("output from line 1 = %+v", v)
	}
	if rec := guiGet(t, h, "127.0.0.1:1234", "/api/jobs/999", g.token); rec.Code != http.StatusNotFound {
		t.Errorf("missing job: %d", rec.Code)
	}

	var st guiStatus
	if err := json.Unmarshal(guiGet(t, h, "127.0.0.1:1234", "/api/status", g.token).Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Jobs) != 1 || st.Jobs[0].ID != started.ID || st.Jobs[0].Lines != nil {
		t.Errorf("status jobs = %+v", st.Jobs)
	}
}

func TestGUIServeRefusedWhenAlreadyRunning(t *testing.T) {
	g, _, _ := guiTestServer(t)
	g.jobs = fakeGUIJobs(t)
	g.health = func(int, string) bool { return true }
	rec := guiPost(t, g.handler(), "/api/jobs", g.token, "", "application/json", `{"action":"serve"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already running") {
		t.Fatalf("got %d %q", rec.Code, rec.Body)
	}
}

func TestGUIStopEndpoint(t *testing.T) {
	g, _, _ := guiTestServer(t)
	g.jobs = fakeGUIJobs(t)
	j, err := g.jobs.start("serve", "")
	if err != nil {
		t.Fatal(err)
	}
	waitForLine(t, j, "started")
	rec := guiPost(t, g.handler(), fmt.Sprintf("/api/jobs/%d/stop", j.id), g.token, "", "application/json", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("stop: %d", rec.Code)
	}
	if v := waitJob(t, j); !v.Stopped || v.Running {
		t.Fatalf("after stop = %+v", v)
	}
}
