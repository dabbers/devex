package workspace

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/vm"
)

// serviceRoot is where a service's pid and log live inside the machine,
// relative to the workspace. Under a dot directory so it does not appear in
// the repository the agent is editing, and inside the workspace so it survives
// a stop along with everything else on the disk.
const serviceRoot = ".dabberz/services"

// How long a service is given to become reachable, and how often it is
// checked. A dev server that compiles on boot can take a while; one that
// exits immediately should be reported quickly, which the liveness check
// below handles rather than this timeout.
const (
	healthTimeout  = 90 * time.Second
	healthInterval = 500 * time.Millisecond
	probeTimeout   = 2 * time.Second
)

// startServices brings a project's services up in dependency order.
//
// Launching goes through "sh -c" deliberately: Command is a shell command line
// as a person would type it, and the driver never interprets argv. setsid puts
// each service in its own process group, which is what makes stopping it
// actually stop it -- a dev server is usually a wrapper that spawns the real
// process, and killing only the wrapper leaves the port held by an orphan.
func (m *Manager) startServices(ctx context.Context, w *domain.Workspace, instance *vm.Instance, services []*domain.Service, env map[string]string) error {
	for _, service := range services {
		if strings.TrimSpace(service.Command) == "" {
			// Nothing to run. A service can legitimately be documentation of
			// something that runs elsewhere.
			continue
		}
		if err := m.startService(ctx, instance, service, env); err != nil {
			return err
		}
		if err := m.awaitHealthy(ctx, instance, service); err != nil {
			return fmt.Errorf("workspace: %s: %w", service.Name, err)
		}
		m.opts.Logger.Info("service is up",
			"workspace", w.ID, "service", service.Name, "port", service.Port)
	}
	return nil
}

// startService launches one service, replacing it if it is already running.
func (m *Manager) startService(ctx context.Context, instance *vm.Instance, service *domain.Service, env map[string]string) error {
	if err := m.stopService(ctx, instance, service); err != nil {
		return err
	}

	dir := serviceRoot + "/" + service.Name
	// The log is appended to rather than truncated: why a service died is
	// usually in the lines before the restart somebody did to fix it.
	//
	// Every interpolation is shell-quoted, not %q. Go's quoting is Go syntax:
	// it yields a double-quoted string, and a command carrying its own quotes
	// -- node --title="my app" -- loses them when the outer shell re-parses
	// it, silently turning one argument into two. Single quotes hand the
	// command to the inner sh byte for byte, and that inner shell is what
	// expands any variables in it, which is what a person writing a service
	// command expects.
	script := fmt.Sprintf(
		`set -e; mkdir -p %s; setsid sh -c %s >> %s 2>&1 < /dev/null & echo $! > %s`,
		shellQuote(dir), shellQuote(service.Command),
		shellQuote(dir+"/log"), shellQuote(dir+"/pid"))

	res, err := m.driver.Exec(ctx, instance.ID, vm.Command{
		Argv:    []string{"sh", "-c", script},
		Env:     env,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("workspace: start %s: %w", service.Name, err)
	}
	if !res.OK() {
		return fmt.Errorf("workspace: start %s: %s", service.Name, firstLine(res.Stderr))
	}
	return nil
}

// stopService ends a service and everything it started.
func (m *Manager) stopService(ctx context.Context, instance *vm.Instance, service *domain.Service) error {
	pidFile := serviceRoot + "/" + service.Name + "/pid"
	// The negative pid targets the process group, so a wrapper's children go
	// with it. TERM first, then KILL for anything that ignores it; both are
	// tolerated failing, because the usual reason is that nothing is running.
	quoted := shellQuote(pidFile)
	script := fmt.Sprintf(
		`if [ -f %s ]; then pid=$(cat %s); kill -TERM -"$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true; `+
			`sleep 0.3; kill -KILL -"$pid" 2>/dev/null || true; rm -f %s; fi`,
		quoted, quoted, quoted)

	if _, err := m.driver.Exec(ctx, instance.ID, vm.Command{
		Argv:    []string{"sh", "-c", script},
		Timeout: 30 * time.Second,
	}); err != nil {
		return fmt.Errorf("workspace: stop %s: %w", service.Name, err)
	}
	return nil
}

// stopServices shuts a project's services down, in reverse dependency order
// so nothing is taken away from something still relying on it.
func (m *Manager) stopServices(ctx context.Context, instance *vm.Instance, services []*domain.Service) {
	for i := len(services) - 1; i >= 0; i-- {
		if strings.TrimSpace(services[i].Command) == "" {
			continue
		}
		if err := m.stopService(ctx, instance, services[i]); err != nil {
			// Best effort: a workspace must be able to stop even if one
			// service will not, or it is stuck holding a machine forever.
			m.opts.Logger.Warn("could not stop a service",
				"service", services[i].Name, "error", err)
		}
	}
}

// awaitHealthy waits for a service to answer.
//
// The probe runs from the control plane against the same address the proxy
// will use, so what is checked is what a visitor would reach, rather than
// something only true from inside the machine.
func (m *Manager) awaitHealthy(ctx context.Context, instance *vm.Instance, service *domain.Service) error {
	if service.Port == 0 {
		// Nothing listens, so there is nothing to wait for. A worker is
		// up when it has been started.
		return nil
	}

	address := net.JoinHostPort(instance.Address, fmt.Sprint(service.Port))
	deadline := time.Now().Add(healthTimeout)
	var last error

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A service that has already exited will never answer, so say that
		// rather than waiting out the full timeout for it.
		if alive, err := m.serviceAlive(ctx, instance, service); err == nil && !alive {
			return fmt.Errorf("it exited while starting; check its log")
		}

		last = m.probe(ctx, address, service.HealthPath)
		if last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("it did not answer on %s within %s: %w", address, healthTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(healthInterval):
		}
	}
}

// probe reports whether a service answers: an HTTP request when it declares a
// health path, otherwise that the port accepts a connection, which is all that
// can be assumed without one.
func (m *Manager) probe(ctx context.Context, address, healthPath string) error {
	if healthPath == "" {
		conn, err := (&net.Dialer{Timeout: probeTimeout}).DialContext(ctx, "tcp", address)
		if err != nil {
			return err
		}
		return conn.Close()
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+healthPath, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		// 4xx is still an answer: a health path that is not routed yet says
		// the server is up, which is what is being waited for.
		return fmt.Errorf("health check returned %s", resp.Status)
	}
	return nil
}

// serviceAlive reports whether the service's process group still exists.
func (m *Manager) serviceAlive(ctx context.Context, instance *vm.Instance, service *domain.Service) (bool, error) {
	pidFile := serviceRoot + "/" + service.Name + "/pid"
	res, err := m.driver.Exec(ctx, instance.ID, vm.Command{
		Argv: []string{"sh", "-c", fmt.Sprintf(`[ -f %s ] && kill -0 "$(cat %s)" 2>/dev/null`,
			shellQuote(pidFile), shellQuote(pidFile))},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		return false, err
	}
	return res.OK(), nil
}

// ServiceStatus is what a service is doing right now.
type ServiceStatus struct {
	Name string `json:"name"`
	// Running reports whether its process group still exists.
	Running bool `json:"running"`
	// Reachable reports whether it answers on its port. A service can be
	// running and not yet reachable, which is what starting looks like.
	Reachable bool   `json:"reachable"`
	Detail    string `json:"detail,omitempty"`
}

// Status reports what each of a workspace's services is doing.
func (m *Manager) Status(ctx context.Context, w *domain.Workspace, services []*domain.Service) ([]ServiceStatus, error) {
	if w.InstanceID == "" {
		return nil, nil
	}
	instance, err := m.driver.Get(ctx, w.InstanceID)
	if err != nil {
		return nil, err
	}

	out := make([]ServiceStatus, 0, len(services))
	for _, service := range services {
		status := ServiceStatus{Name: service.Name}
		if strings.TrimSpace(service.Command) == "" {
			status.Detail = "nothing to run"
			out = append(out, status)
			continue
		}
		alive, err := m.serviceAlive(ctx, instance, service)
		if err != nil {
			status.Detail = err.Error()
			out = append(out, status)
			continue
		}
		status.Running = alive
		if alive && service.Port > 0 {
			address := net.JoinHostPort(instance.Address, fmt.Sprint(service.Port))
			if err := m.probe(ctx, address, service.HealthPath); err != nil {
				status.Detail = firstLine(err.Error())
			} else {
				status.Reachable = true
			}
		}
		out = append(out, status)
	}
	return out, nil
}

// Logs returns the tail of a service's log.
func (m *Manager) Logs(ctx context.Context, w *domain.Workspace, serviceName string, lines int) (string, error) {
	if w.InstanceID == "" {
		return "", fmt.Errorf("workspace: %s is not running", w.Name)
	}
	if lines <= 0 || lines > 2000 {
		lines = 200
	}
	logFile := serviceRoot + "/" + serviceName + "/log"
	res, err := m.driver.Exec(ctx, w.InstanceID, vm.Command{
		Argv:    []string{"sh", "-c", fmt.Sprintf(`tail -n %d %s 2>/dev/null || true`, lines, shellQuote(logFile))},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// shellQuote wraps a string so a shell receives it as one literal argument.
//
// Single quotes because they suppress every expansion the shell would
// otherwise do. A single quote inside the string is closed, escaped and
// reopened, which is the only way to represent one.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
