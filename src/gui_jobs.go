package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// The dashboard's buttons run chainlink itself as a child process with a fixed
// argument list, and the page polls the output. Only the actions below can be
// run; the page names one and never supplies arguments.

// guiAction is a command the dashboard may run.
type guiAction struct {
	Args []string
	// Background actions (serve) run until stopped and don't block the others.
	Background bool
	// Input actions read one line from stdin, so a secret never appears in
	// the process list.
	Input bool
}

var guiActions = map[string]guiAction{
	"init":         {Args: []string{"init"}},
	"install":      {Args: []string{"install", "-y"}}, // the click is the confirmation
	"assets":       {Args: []string{"assets"}},
	"serve":        {Args: []string{"serve"}, Background: true},
	"self-install": {Args: []string{"self-install"}},
	"login-cloud":  {Args: []string{"login-cloud"}, Input: true},
	"update":       {Args: []string{"update"}},
}

const (
	guiMaxJobs     = 20   // finished jobs kept for the page
	guiMaxLines    = 5000 // output lines kept per job
	guiStopTimeout = 3 * time.Second
)

var (
	errGUIBusy          = errors.New("another command is still running")
	errGUIUnknownAction = errors.New("unknown action")
)

type guiJob struct {
	id      int
	action  string
	command string // as shown to the user
	started time.Time

	mu       sync.Mutex
	lines    []string
	dropped  int // lines trimmed from the front
	running  bool
	exitCode int
	err      string
	stopped  bool
	cancel   context.CancelFunc
	done     chan struct{}
}

// guiJobView is a job as the page sees it. Lines and Next are set only when
// output is requested.
type guiJobView struct {
	ID       int       `json:"id"`
	Action   string    `json:"action"`
	Command  string    `json:"command"`
	Started  time.Time `json:"started"`
	Running  bool      `json:"running"`
	ExitCode int       `json:"exitCode"`
	Error    string    `json:"error,omitempty"`
	Stopped  bool      `json:"stopped,omitempty"`
	From     int       `json:"from"`
	Lines    []string  `json:"lines,omitempty"`
	Next     int       `json:"next"`
}

func (j *guiJob) view(from int, withLines bool) guiJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := guiJobView{
		ID: j.id, Action: j.action, Command: j.command, Started: j.started,
		Running: j.running, ExitCode: j.exitCode, Error: j.err, Stopped: j.stopped,
		Next: j.dropped + len(j.lines),
	}
	if withLines {
		from = max(from, j.dropped)
		if i := from - j.dropped; i < len(j.lines) {
			v.Lines = slices.Clone(j.lines[i:])
		}
		v.From = from
	}
	return v
}

func (j *guiJob) appendLine(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, line)
	if over := len(j.lines) - guiMaxLines; over > 0 {
		j.lines = j.lines[over:]
		j.dropped += over
	}
}

// guiJobs runs and remembers the dashboard's commands.
type guiJobs struct {
	// command builds the child process; tests swap in a fake chainlink.
	command func(ctx context.Context, args ...string) (*exec.Cmd, error)

	mu     sync.Mutex
	jobs   []*guiJob // oldest first
	nextID int
}

func newGUIJobs() *guiJobs {
	return &guiJobs{command: chainlinkCommand}
}

// chainlinkCommand runs this chainlink binary. A binary named anything but
// chainlink would start as a tool proxy or the first-run installer instead.
func chainlinkCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if mode, _ := classifyInvocation(exe); mode != invokeCLI {
		return nil, fmt.Errorf("can't run commands from %s; run 'chainlink ui' from an installed chainlink", exe)
	}
	return exec.CommandContext(ctx, exe, args...), nil
}

func (s *guiJobs) find(id int) *guiJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.id == id {
			return j
		}
	}
	return nil
}

func (s *guiJobs) views() []guiJobView {
	s.mu.Lock()
	jobs := slices.Clone(s.jobs)
	s.mu.Unlock()
	out := make([]guiJobView, 0, len(jobs))
	for i := len(jobs) - 1; i >= 0; i-- {
		out = append(out, jobs[i].view(0, false))
	}
	return out
}

// runningLocked reports whether a job for action, or any foreground job when
// action is "", is running. s.mu must be held.
func (s *guiJobs) runningLocked(action string) bool {
	for _, j := range s.jobs {
		j.mu.Lock()
		r := j.running
		j.mu.Unlock()
		if !r {
			continue
		}
		if action != "" && j.action == action {
			return true
		}
		if action == "" && !guiActions[j.action].Background {
			return true
		}
	}
	return false
}

// start runs action. input is written to stdin for Input actions.
func (s *guiJobs) start(action, input string) (*guiJob, error) {
	a, ok := guiActions[action]
	if !ok {
		return nil, errGUIUnknownAction
	}
	if a.Input && strings.TrimSpace(input) == "" {
		return nil, errors.New("this action needs a value")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if a.Background && s.runningLocked(action) {
		return nil, fmt.Errorf("chainlink %s is already running", action)
	}
	if !a.Background && s.runningLocked("") {
		return nil, errGUIBusy
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := s.command(ctx, a.Args...)
	if err != nil {
		cancel()
		return nil, err
	}
	// Ask nicely first so serve and downloads clean up; Kill after WaitDelay.
	if runtime.GOOS != "windows" {
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	}
	cmd.WaitDelay = guiStopTimeout
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "CHAINLINK_NO_UPDATE_CHECK=1")
	if a.Input {
		cmd.Stdin = strings.NewReader(strings.TrimSpace(input) + "\n")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw

	s.nextID++
	j := &guiJob{
		id:      s.nextID,
		action:  action,
		command: "chainlink " + strings.Join(a.Args, " "),
		started: time.Now(),
		running: true,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	if err := cmd.Start(); err != nil {
		cancel()
		pr.Close()
		pw.Close()
		return nil, err
	}
	pw.Close() // the child has its own copy

	s.jobs = append(s.jobs, j)
	s.trimLocked()

	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			j.appendLine(sc.Text())
		}
		if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.EOF) {
			j.appendLine("(output cut off: " + err.Error() + ")")
		}
	}()
	go func() {
		err := cmd.Wait()
		// Let the reader drain what the child wrote last; a grandchild still
		// holding the pipe shouldn't keep the job running forever.
		select {
		case <-scanned:
		case <-time.After(2 * time.Second):
		}
		pr.Close()
		cancel()

		j.mu.Lock()
		j.running = false
		j.exitCode = cmd.ProcessState.ExitCode()
		if err != nil && !j.stopped {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				j.err = err.Error()
			}
		}
		j.mu.Unlock()
		close(j.done)
	}()
	return j, nil
}

// trimLocked forgets the oldest finished jobs beyond guiMaxJobs.
func (s *guiJobs) trimLocked() {
	for len(s.jobs) > guiMaxJobs {
		i := -1
		for k, j := range s.jobs {
			j.mu.Lock()
			r := j.running
			j.mu.Unlock()
			if !r {
				i = k
				break
			}
		}
		if i < 0 {
			return
		}
		s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
	}
}

func (j *guiJob) stop() {
	j.mu.Lock()
	if j.running {
		j.stopped = true
	}
	j.mu.Unlock()
	j.cancel()
}

// stopAll stops every running job and waits for them, up to timeout.
func (s *guiJobs) stopAll(timeout time.Duration) {
	s.mu.Lock()
	jobs := slices.Clone(s.jobs)
	s.mu.Unlock()
	deadline := time.After(timeout)
	for _, j := range jobs {
		j.stop()
	}
	for _, j := range jobs {
		select {
		case <-j.done:
		case <-deadline:
			return
		}
	}
}
