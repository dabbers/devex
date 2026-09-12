package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/llm"
)

// discoverResponse is the JSON shape the discovery pass is asked for.
type discoverResponse struct {
	Projects []struct {
		Name           string `json:"name"`
		Path           string `json:"path"`
		Toolchain      string `json:"toolchain"`
		PreviewCommand string `json:"preview_command"`
		PreviewPort    int    `json:"preview_port"`
	} `json:"projects"`
}

// DiscoverProjects infers which projects a repo contains from its layout.
//
// No manifest file is required: a model pass reads the tree and proposes the
// projects it sees. The results land unconfirmed, because the user confirms
// them once per repo; what is learned is then kept in the repo's memory store
// so later passes do not start from nothing.
func (o *Orchestrator) DiscoverProjects(ctx context.Context, repoID string, tree []string) ([]*domain.Project, error) {
	repo, err := o.store.GetRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if len(tree) == 0 {
		return nil, errors.New("orchestrator: discovery needs a file listing")
	}

	temperature := 0.0
	resp, err := o.model.Complete(ctx, llm.Request{
		Messages: []llm.Message{
			llm.System(discoverSystemPrompt),
			llm.User(discoverUserPrompt(repo, tree)),
		},
		Temperature: &temperature,
		JSON:        true,
	})
	if err != nil {
		return nil, fmt.Errorf("orchestrator: discovery call failed: %w", err)
	}
	if resp.Truncated() {
		return nil, errors.New("orchestrator: the discovery response was cut off at the token limit")
	}

	var decoded discoverResponse
	if err := llm.DecodeJSON(resp.Content, &decoded); err != nil {
		return nil, fmt.Errorf("orchestrator: discovery response: %w", err)
	}

	projects := make([]*domain.Project, 0, len(decoded.Projects))
	seen := map[string]bool{}
	for _, p := range decoded.Projects {
		path := normalizeProjectPath(p.Path)
		if seen[path] {
			continue
		}
		seen[path] = true

		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = projectNameFromPath(path)
		}
		project := &domain.Project{
			RepoID:         repoID,
			Name:           name,
			Path:           path,
			Toolchain:      strings.ToLower(strings.TrimSpace(p.Toolchain)),
			PreviewCommand: strings.TrimSpace(p.PreviewCommand),
			PreviewPort:    p.PreviewPort,
			// Discovery proposes; the user confirms. Nothing downstream should
			// treat an inferred layout as settled.
			Confirmed: false,
		}
		// Upsert by path so rerunning discovery refines the existing records
		// instead of duplicating them.
		if err := o.store.UpsertProject(ctx, project); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}

	if len(projects) == 0 {
		// A repo always contains at least one project, even if the model could
		// not name it; falling back keeps the repo usable.
		fallback := &domain.Project{RepoID: repoID, Name: "root", Path: "."}
		if err := o.store.UpsertProject(ctx, fallback); err != nil {
			return nil, err
		}
		projects = append(projects, fallback)
	}
	return projects, nil
}

// ConfirmProjects marks a repo's discovered layout as accepted by the user.
func (o *Orchestrator) ConfirmProjects(ctx context.Context, repoID string, confirmedPaths []string) ([]*domain.Project, error) {
	projects, err := o.store.ListProjects(ctx, repoID)
	if err != nil {
		return nil, err
	}

	keep := make(map[string]bool, len(confirmedPaths))
	for _, path := range confirmedPaths {
		keep[normalizeProjectPath(path)] = true
	}

	confirmed := make([]*domain.Project, 0, len(projects))
	for _, project := range projects {
		// With no explicit list, every discovered project is accepted.
		project.Confirmed = len(confirmedPaths) == 0 || keep[project.Path]
		if err := o.store.UpdateProject(ctx, project); err != nil {
			return nil, err
		}
		if project.Confirmed {
			confirmed = append(confirmed, project)
		}
	}

	repo, err := o.store.GetRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	repo.Discovered = true
	if err := o.store.UpdateRepo(ctx, repo); err != nil {
		return nil, err
	}
	return confirmed, nil
}

// normalizeProjectPath reduces a reported path to the store's convention: a
// clean repo-relative path, with the repo root written as ".".
func normalizeProjectPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.Trim(path, "/")
	path = strings.TrimPrefix(path, "./")
	if path == "" || path == "." {
		return "."
	}
	return path
}
