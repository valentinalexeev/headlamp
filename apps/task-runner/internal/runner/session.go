package runner

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

const (
	defaultSessionPort = 6080
	defaultSessionTTL  = 3600
	minSessionTTL      = 60
	maxSessionTTL      = 14400
	defaultSessionIdle = 900
	minSessionIdle     = 60
	maxSessionIdle     = 3600
	keepEndedSession   = 5 * time.Minute
)

// session is one interactive browser container. Like a run it holds the
// node's single slot (Server.activeSession) until its container is reaped.
type session struct {
	id       string
	state    string // running | ended
	reason   string // ttl | idle | stopped | failed, once ended
	started  time.Time
	expires  time.Time
	finished time.Time
	port     int
	idle     int
	stopCh   chan struct{}
	done     chan struct{}
}

// normalizeBind returns the host part for `-p`: a valid, specific IP (IPv6
// in brackets). An empty, unparsable or unspecified address would publish on
// every interface, so it is refused.
func normalizeBind(bind string) (string, error) {
	ip := net.ParseIP(bind)
	if ip == nil || ip.IsUnspecified() {
		return "", errors.New("RUNNER_SESSION_BIND must be a specific IP address")
	}
	if ip.To4() == nil {
		return "[" + ip.String() + "]", nil
	}
	return ip.String(), nil
}

type sessionRequest struct {
	TTLSeconds  *int `json:"ttl_seconds"`
	IdleSeconds *int `json:"idle_seconds"`
}

func (s *Server) sessionArgs(sess *session, ttl time.Duration) []string {
	port := strconv.Itoa(s.cfg.SessionPort)
	args := append([]string(nil), globalFlags...)
	return append(args,
		"run", "--rm", "--init", "--name", "task-"+sess.id,
		"--timeout", strconv.Itoa(int((ttl+s.cfg.KillGrace+time.Second-1)/time.Second)),
		"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(s.cfg.PidsLimit), "--memory", s.cfg.Memory,
		"--tmpfs", "/tmp", "--tmpfs", "/home/pwuser:rw", "--shm-size=512m",
		"-p", s.bindHost+":"+port+":"+port,
		"-e", "SESSION_IDLE_SECONDS", "-e", "SESSION_NOVNC_PORT",
		s.cfg.Image, "sh", "/app/session.sh")
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	if s.bindHost == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var req sessionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) || dec.Decode(&struct{}{}) != io.EOF {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	ttl, idle := defaultSessionTTL, defaultSessionIdle
	if req.TTLSeconds != nil {
		ttl = *req.TTLSeconds
	}
	if req.IdleSeconds != nil {
		idle = *req.IdleSeconds
	}
	if ttl < minSessionTTL || ttl > maxSessionTTL || idle < minSessionIdle || idle > maxSessionIdle {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	ttlDur := time.Duration(ttl) * s.ttlUnit
	now := time.Now()
	sess := &session{
		id: newID(), state: "running", started: now, expires: now.Add(ttlDur),
		port: s.cfg.SessionPort, idle: idle, stopCh: make(chan struct{}), done: make(chan struct{}),
	}
	s.mu.Lock()
	if s.active != nil || s.activeSession != nil {
		s.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		return
	}
	s.activeSession = sess
	s.lastSession = sess
	s.mu.Unlock()

	s.log.Info("session started", "session", sess.id, "ttl_s", ttl, "idle_s", idle)
	go func() {
		reason := s.runSession(sess, ttlDur)
		s.mu.Lock()
		if sess.state == "running" { // a stop already published its state
			sess.state, sess.reason = "ended", reason
		}
		sess.finished = time.Now()
		s.activeSession = nil
		final := sess.reason
		s.mu.Unlock()
		close(sess.done)
		s.log.Info("session finished", "session", sess.id, "reason", final)
	}()
	writeJSON(w, 201, map[string]any{
		"id": sess.id, "port": sess.port,
		"expires_at": sess.expires.UTC().Format(time.RFC3339),
	})
}

// runSession runs the container until it exits, the TTL passes or a stop
// arrives, and returns the end reason. Stopping mirrors execute: kill by name,
// process-group SIGKILL after KillGrace, then remove the container by name.
func (s *Server) runSession(sess *session, ttl time.Duration) string {
	cmd := exec.Command(s.cfg.ContainerBin, s.sessionArgs(sess, ttl)...)
	cmd.Env = append(childEnv("", ""),
		"SESSION_IDLE_SECONDS="+strconv.Itoa(sess.idle),
		"SESSION_NOVNC_PORT="+strconv.Itoa(sess.port))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		s.log.Error("session start failed", "session", sess.id, "err", err.Error())
		return "failed"
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(time.Until(sess.expires))
	defer timer.Stop()
	reason := ""
	select {
	case err := <-done:
		if err != nil {
			return "failed"
		}
		return "idle"
	case <-timer.C:
		reason = "ttl"
	case <-sess.stopCh:
		reason = "stopped"
	}
	s.killByName(sess.id)
	select {
	case <-done:
	case <-time.After(s.cfg.KillGrace):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	s.removeByName(sess.id)
	return reason
}

// stopLocked ends a running session at once for the API; the slot stays taken
// until the container is reaped. Caller holds s.mu.
func (s *Server) stopLocked(sess *session) {
	if sess.state == "running" {
		sess.state, sess.reason = "ended", "stopped"
		close(sess.stopCh)
	}
}

// currentSession returns the last session unless it ended more than
// keepEndedSession ago. Caller holds s.mu.
func (s *Server) currentSession() *session {
	sess := s.lastSession
	if sess != nil && !sess.finished.IsZero() && time.Since(sess.finished) > keepEndedSession {
		s.lastSession = nil
		return nil
	}
	return sess
}

func (s *Server) sessionStatus(w http.ResponseWriter) {
	if s.bindHost == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.mu.Lock()
	sess := s.currentSession()
	var view map[string]any
	if sess != nil {
		view = map[string]any{
			"id": sess.id, "state": sess.state, "port": sess.port, "reason": sess.reason,
			"started_at": sess.started.UTC().Format(time.RFC3339),
			"expires_at": sess.expires.UTC().Format(time.RFC3339),
		}
	}
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"session": view})
}

func (s *Server) stopSession(w http.ResponseWriter, id string) {
	if s.bindHost == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.mu.Lock()
	sess := s.currentSession()
	if sess == nil || sess.id != id {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.stopLocked(sess)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
