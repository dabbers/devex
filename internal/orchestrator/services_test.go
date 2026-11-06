package orchestrator

import (
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/domain"
)

// byName indexes a repaired service set for assertions.
func byName(services []*domain.Service) map[string]*domain.Service {
	out := make(map[string]*domain.Service, len(services))
	for _, sv := range services {
		out[sv.Name] = sv
	}
	return out
}

func TestDiscoveryElectsOneFrontDoorAndSaysWhichLost(t *testing.T) {
	services, warnings := servicesFromDiscovery("proj_1", []discoveredService{
		{Name: "admin", Kind: "web", Command: "npm run admin", Port: 4000},
		{Name: "web", Kind: "web", Command: "npm run dev", Port: 3000},
	})

	primaries := 0
	for _, sv := range services {
		if sv.Primary {
			primaries++
		}
	}
	if primaries != 1 {
		t.Fatalf("got %d front doors, want exactly 1: %+v", primaries, services)
	}
	// A service literally named "web" wins, so reruns pick the same one rather
	// than whichever the model happened to emit first.
	if !byName(services)["web"].Primary {
		t.Fatalf("the service named web should be the front door: %+v", services)
	}
	if !byName(services)["admin"].Exposed {
		t.Fatal("losing the election should not make a service unreachable")
	}
	if !containsSubstring(warnings, "picked \"web\" as the front door") {
		t.Fatalf("the choice was not recorded for the user: %v", warnings)
	}
}

func TestDiscoveryNeverExposesADatastore(t *testing.T) {
	services, warnings := servicesFromDiscovery("proj_1", []discoveredService{
		{Name: "postgres", Kind: "datastore", Command: "postgres", Port: 5432},
	})

	sv := byName(services)["postgres"]
	if sv == nil {
		t.Fatalf("the datastore was dropped: %+v", services)
	}
	// The port is real information and is kept; what must not happen is a
	// publicly reachable database.
	if sv.Port != 5432 {
		t.Fatalf("the datastore lost its port: %+v", sv)
	}
	if sv.Exposed {
		t.Fatalf("a datastore was given a public hostname: %+v", sv)
	}
	if !containsSubstring(warnings, "not published publicly") {
		t.Fatalf("the decision was not explained: %v", warnings)
	}
}

func TestDiscoveryRecategorisesAWorkerThatListens(t *testing.T) {
	services, warnings := servicesFromDiscovery("proj_1", []discoveredService{
		{Name: "jobs", Kind: "worker", Command: "./jobs", Port: 9000},
	})

	sv := byName(services)["jobs"]
	if sv.Kind != domain.ServiceAPI {
		t.Fatalf("a worker with a port should become an api: %+v", sv)
	}
	if !sv.Exposed {
		t.Fatalf("the recategorised service should be reachable: %+v", sv)
	}
	if !containsSubstring(warnings, "treated as an api") {
		t.Fatalf("the change was not recorded: %v", warnings)
	}
}

func TestDiscoveryDropsDependenciesOnServicesItNeverListed(t *testing.T) {
	services, warnings := servicesFromDiscovery("proj_1", []discoveredService{
		{Name: "web", Kind: "web", Command: "npm run dev", Port: 3000,
			DependsOn: []string{"api", "redis", "web"}},
		{Name: "api", Kind: "api", Command: "go run .", Port: 8080},
	})

	web := byName(services)["web"]
	if len(web.DependsOn) != 1 || web.DependsOn[0] != "api" {
		t.Fatalf("dependencies were not pruned to the real set: %+v", web.DependsOn)
	}
	if !containsSubstring(warnings, "which this project does not have") {
		t.Fatalf("the dropped dependency was not reported: %v", warnings)
	}
	if !containsSubstring(warnings, "depended on itself") {
		t.Fatalf("the self-dependency was not reported: %v", warnings)
	}
}

func TestDiscoveryBreaksCyclesSoSomethingCanStart(t *testing.T) {
	services, warnings := servicesFromDiscovery("proj_1", []discoveredService{
		{Name: "web", Kind: "web", Command: "a", Port: 3000, DependsOn: []string{"api"}},
		{Name: "api", Kind: "api", Command: "b", Port: 8080, DependsOn: []string{"web"}},
	})

	// A cycle means nothing can ever start, so the set must be repaired rather
	// than stored and discovered at provision time.
	if _, err := domain.StartOrder(services); err != nil {
		t.Fatalf("the repaired set still cannot be started: %v", err)
	}
	if !containsSubstring(warnings, "cycle") {
		t.Fatalf("breaking the cycle was not reported: %v", warnings)
	}
}

func TestDiscoveryNormalisesNamesAndDropsDuplicates(t *testing.T) {
	services, warnings := servicesFromDiscovery("proj_1", []discoveredService{
		{Name: "  Web App  ", Kind: "web", Command: "a", Port: 3000},
		{Name: "web-app", Kind: "web", Command: "b", Port: 3001},
		{Name: "!!!", Kind: "web", Command: "c", Port: 3002},
	})

	if len(services) != 1 {
		t.Fatalf("got %d services, want 1 after normalisation: %+v", len(services), services)
	}
	if services[0].Name != "web-app" {
		t.Fatalf("name was not slugged for use in a hostname: %q", services[0].Name)
	}
	if !containsSubstring(warnings, "also named") {
		t.Fatalf("the duplicate was not reported: %v", warnings)
	}
	if !containsSubstring(warnings, "no usable name") {
		t.Fatalf("the unnameable service was not reported: %v", warnings)
	}
}

func TestDiscoveryLeavesABackendOnlyProjectWithNoFrontDoor(t *testing.T) {
	services, _ := servicesFromDiscovery("proj_1", []discoveredService{
		{Name: "api", Kind: "api", Command: "go run .", Port: 8080},
		{Name: "jobs", Kind: "worker", Command: "./jobs"},
	})

	// Inventing a front door would hand the verifier a URL that serves JSON.
	if primary := domain.PrimaryService(services); primary != nil {
		t.Fatalf("a backend-only project was given a front door: %+v", primary)
	}
	if !byName(services)["api"].Exposed {
		t.Fatal("the api should still be reachable")
	}
}

func TestDiscoveryGuessesAKindRatherThanFailing(t *testing.T) {
	services, warnings := servicesFromDiscovery("proj_1", []discoveredService{
		{Name: "thing", Kind: "daemon", Command: "./thing", Port: 7000},
		{Name: "quiet", Kind: "", Command: "./quiet"},
	})

	index := byName(services)
	if index["thing"].Kind != domain.ServiceAPI {
		t.Fatalf("an unknown kind that listens should become an api: %+v", index["thing"])
	}
	if index["quiet"].Kind != domain.ServiceWorker {
		t.Fatalf("an unknown kind that listens on nothing should become a worker: %+v", index["quiet"])
	}
	if !containsSubstring(warnings, "unknown kind") {
		t.Fatalf("the guess was not recorded: %v", warnings)
	}
}

func TestDiscoveryClearsAnImpossiblePort(t *testing.T) {
	services, warnings := servicesFromDiscovery("proj_1", []discoveredService{
		{Name: "web", Kind: "web", Command: "a", Port: 99999},
	})

	if services[0].Port != 0 || services[0].Exposed {
		t.Fatalf("an out-of-range port was kept: %+v", services[0])
	}
	if !containsSubstring(warnings, "outside 1-65535") {
		t.Fatalf("the cleared port was not reported: %v", warnings)
	}
}

func TestDiscoveryMakesHealthPathsAbsolute(t *testing.T) {
	services, _ := servicesFromDiscovery("proj_1", []discoveredService{
		{Name: "web", Kind: "web", Command: "a", Port: 3000, HealthPath: "healthz"},
		{Name: "api", Kind: "api", Command: "b", Port: 8080, HealthPath: "  "},
	})

	index := byName(services)
	if index["web"].HealthPath != "/healthz" {
		t.Fatalf("health path was not made absolute: %q", index["web"].HealthPath)
	}
	if index["api"].HealthPath != "" {
		t.Fatalf("a blank health path should stay empty, got %q", index["api"].HealthPath)
	}
}

// TestRepairedSetIsAlwaysStorable is the property that matters: whatever the
// model proposes, the result must satisfy every rule the store enforces.
func TestRepairedSetIsAlwaysStorable(t *testing.T) {
	proposals := [][]discoveredService{
		{{Name: "web", Kind: "web", Port: 3000}, {Name: "web", Kind: "web", Port: 3001}},
		{{Name: "a", Kind: "web", Port: 1, DependsOn: []string{"b"}},
			{Name: "b", Kind: "web", Port: 2, DependsOn: []string{"c"}},
			{Name: "c", Kind: "web", Port: 3, DependsOn: []string{"a"}}},
		{{Name: "db", Kind: "datastore", Port: 5432, DependsOn: []string{"db"}}},
		{{Name: "", Kind: "", Port: -5}},
		{},
	}
	for i, proposed := range proposals {
		services, _ := servicesFromDiscovery("proj_1", proposed)
		if _, err := domain.StartOrder(services); err != nil {
			t.Errorf("proposal %d produced an unstartable set: %v", i, err)
		}
		primaries := 0
		for _, sv := range services {
			if err := sv.Validate(); err != nil {
				t.Errorf("proposal %d produced an invalid service: %v", i, err)
			}
			if sv.Primary {
				primaries++
			}
		}
		if primaries > 1 {
			t.Errorf("proposal %d produced %d front doors", i, primaries)
		}
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
