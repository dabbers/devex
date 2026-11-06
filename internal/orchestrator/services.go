package orchestrator

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dabbers/devex/internal/domain"
)

// discoveredService is one service as the discovery pass proposed it: raw
// model output, not yet checked against anything.
type discoveredService struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Command    string   `json:"command"`
	Port       int      `json:"port"`
	HealthPath string   `json:"health_path"`
	DependsOn  []string `json:"depends_on"`
}

// servicesFromDiscovery turns a proposed service set into one the store will
// accept, and reports every correction it had to make.
//
// Discovery is a model pass over a file listing, so it will propose sets that
// cannot be stored: two front doors, an exposed database, a worker with a
// port, a dependency on a service it never listed, a cycle. Rejecting the
// whole set would leave the repo with no services at all, which is worse than
// a repaired set the user is then asked to confirm. So each problem is fixed
// and named, and the names reach the audit trail.
func servicesFromDiscovery(projectID string, proposed []discoveredService) ([]*domain.Service, []string) {
	var warnings []string
	note := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	services := make([]*domain.Service, 0, len(proposed))
	byName := map[string]bool{}

	for _, p := range proposed {
		name := slugName(p.Name)
		if name == "" {
			note("dropped a service with no usable name")
			continue
		}
		if byName[name] {
			note("dropped a second service also named %q", name)
			continue
		}

		kind := domain.ServiceKind(strings.ToLower(strings.TrimSpace(p.Kind)))
		if !kind.Valid() {
			// Guessing from the port would be a guess on a guess. A service
			// that listens is at least an api; one that does not is a worker.
			guessed := domain.ServiceWorker
			if p.Port > 0 {
				guessed = domain.ServiceAPI
			}
			note("service %q had unknown kind %q; treated as %s", name, p.Kind, guessed)
			kind = guessed
		}

		port := p.Port
		if port < 0 || port > 65535 {
			note("service %q had port %d outside 1-65535; cleared it", name, port)
			port = 0
		}
		if kind == domain.ServiceWorker && port > 0 {
			// A worker that listens is not a worker. Believing the port is
			// better than discarding it: the command is what actually runs.
			note("service %q was called a worker but listens on port %d; treated as an api",
				name, port)
			kind = domain.ServiceAPI
		}

		exposed := kind.Exposable() && port > 0
		if kind == domain.ServiceDatastore && port > 0 {
			note("service %q is a datastore; it keeps port %d but is not published publicly",
				name, port)
		}

		services = append(services, &domain.Service{
			ProjectID:  projectID,
			Name:       name,
			Kind:       kind,
			Command:    strings.TrimSpace(p.Command),
			Port:       port,
			HealthPath: normalizeHealthPath(p.HealthPath),
			Exposed:    exposed,
			DependsOn:  p.DependsOn,
			Confirmed:  false,
		})
		byName[name] = true
	}

	pruneDependencies(services, byName, note)
	electFrontDoor(services, note)
	breakCycles(services, note)
	return services, warnings
}

// pruneDependencies drops references to services that are not in the set.
func pruneDependencies(services []*domain.Service, byName map[string]bool, note func(string, ...any)) {
	for _, sv := range services {
		kept := make([]string, 0, len(sv.DependsOn))
		seen := map[string]bool{}
		for _, raw := range sv.DependsOn {
			dep := slugName(raw)
			switch {
			case dep == "":
				continue
			case dep == sv.Name:
				note("service %q depended on itself; dropped that", sv.Name)
			case !byName[dep]:
				note("service %q depended on %q, which this project does not have; dropped that",
					sv.Name, dep)
			case seen[dep]:
				continue
			default:
				seen[dep] = true
				kept = append(kept, dep)
			}
		}
		sv.DependsOn = kept
	}
}

// electFrontDoor makes exactly one web service primary, or none when the
// project has nothing to show a person.
func electFrontDoor(services []*domain.Service, note func(string, ...any)) {
	var candidates []*domain.Service
	for _, sv := range services {
		if sv.Kind == domain.ServiceWeb && sv.Exposed {
			candidates = append(candidates, sv)
		}
	}
	if len(candidates) == 0 {
		// Legitimate: an API-only or worker-only project has no front door,
		// and inventing one would hand the verifier a URL serving JSON.
		return
	}
	// Prefer a service literally called "web", then the lowest port, then the
	// name, so the choice is stable across reruns rather than depending on the
	// order the model happened to emit.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if (a.Name == "web") != (b.Name == "web") {
			return a.Name == "web"
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Name < b.Name
	})
	candidates[0].Primary = true
	if len(candidates) > 1 {
		names := make([]string, 0, len(candidates)-1)
		for _, sv := range candidates[1:] {
			names = append(names, sv.Name)
		}
		note("picked %q as the front door; %s stay reachable but are not the default preview",
			candidates[0].Name, strings.Join(names, ", "))
	}
}

// breakCycles drops dependency edges until the set can be started in some
// order, because a cycle means nothing can start at all.
func breakCycles(services []*domain.Service, note func(string, ...any)) {
	for {
		if _, err := domain.StartOrder(services); err == nil {
			return
		}
		// Drop the last edge of the first service that still has one. This is
		// crude, but a cycle is already a wrong answer: the point is to reach
		// a startable set and tell the user which edge was sacrificed.
		dropped := false
		for _, sv := range services {
			if len(sv.DependsOn) > 0 {
				lost := sv.DependsOn[len(sv.DependsOn)-1]
				sv.DependsOn = sv.DependsOn[:len(sv.DependsOn)-1]
				note("services depended on each other in a cycle; dropped %q -> %q to break it",
					sv.Name, lost)
				dropped = true
				break
			}
		}
		if !dropped {
			// No edges left, so StartOrder failed for a reason a cycle cannot
			// explain. Leave it: the store will refuse the set and say why.
			return
		}
	}
}

// slugName reduces a proposed service name to the lowercase, hyphenated form
// used in hostnames and dependency references.
func slugName(raw string) string {
	var b strings.Builder
	lastHyphen := true
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// normalizeHealthPath makes a proposed health check path absolute, or empty.
func normalizeHealthPath(raw string) string {
	path := strings.TrimSpace(raw)
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}
