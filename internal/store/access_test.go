package store

import (
	"reflect"
	"testing"

	"github.com/dabbers/devex/internal/domain"
)

// TestTheSiteHasItsPolicyAndItsPreviewsAreShut: the edge is regenerated from
// ListOwnedRoutes, so the serving workspace's policy has to come out of it,
// and no change's preview may ever inherit an open one: that is unapproved
// code on a copy of real data.
func TestTheSiteHasItsPolicyAndItsPreviewsAreShut(t *testing.T) {
	s, ctx := newTestStore(t)
	u, repo, project := seed(t, s, ctx)
	sv := seedService(t, s, ctx, project.ID)

	forms := domain.Access{Mode: domain.AccessHybrid, Public: []string{"/f/", "/healthz"}}
	installation := &domain.Installation{
		UserID: u.ID, App: "forms", AppVersion: "0.1.0", Name: "intake",
		RepoID: repo.ID, ProjectID: project.ID, Access: forms,
	}
	if err := s.CreateInstallation(ctx, installation); err != nil {
		t.Fatalf("CreateInstallation: %v", err)
	}

	// A second repository with no installation: a plain workspace.
	plain := &domain.Repo{UserID: u.ID, Name: "plain", RemoteURL: "git@github.com:dabbers/plain.git"}
	if err := s.CreateRepo(ctx, plain); err != nil {
		t.Fatal(err)
	}
	plainProject := &domain.Project{RepoID: plain.ID, Name: "web", Path: ".", Toolchain: "node"}
	if err := s.CreateProject(ctx, plainProject); err != nil {
		t.Fatal(err)
	}
	plainService := seedService(t, s, ctx, plainProject.ID)

	route := func(name, repoID, projectID, serviceID string, port int) string {
		t.Helper()
		ws := &domain.Workspace{UserID: u.ID, RepoID: repoID, ProjectID: projectID, Name: name, Branch: name}
		if err := s.CreateWorkspace(ctx, ws); err != nil {
			t.Fatalf("CreateWorkspace: %v", err)
		}
		if err := s.PutPreviewRoute(ctx, &PreviewRoute{
			WorkspaceID: ws.ID, ServiceID: serviceID, Hostname: name + ".ondabberz.link",
			UpstreamHost: "10.0.0.5", UpstreamPort: port,
		}); err != nil {
			t.Fatalf("PutPreviewRoute: %v", err)
		}
		return ws.ID
	}
	installation.WorkspaceID = route("serving", repo.ID, project.ID, sv.ID, 41000)
	if err := s.UpdateInstallation(ctx, installation); err != nil {
		t.Fatalf("UpdateInstallation: %v", err)
	}
	route("change", repo.ID, project.ID, sv.ID, 41001)
	route("plain", plain.ID, plainProject.ID, plainService.ID, 41002)

	check := func(want map[string]domain.Access) {
		t.Helper()
		routes, err := s.ListOwnedRoutes(ctx)
		if err != nil {
			t.Fatalf("ListOwnedRoutes: %v", err)
		}
		got := map[string]domain.Access{}
		for _, r := range routes {
			got[r.Hostname] = r.Access
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("access by hostname:\n got %+v\nwant %+v", got, want)
		}
	}
	public := domain.Access{Mode: domain.AccessPublic}
	shut := domain.Access{Mode: domain.AccessInternal}
	check(map[string]domain.Access{
		"serving.ondabberz.link": forms,
		"change.ondabberz.link":  shut,
		"plain.ondabberz.link":   public,
	})

	// An open site does not open its previews.
	installation.Access = public
	if err := s.UpdateInstallation(ctx, installation); err != nil {
		t.Fatalf("UpdateInstallation: %v", err)
	}
	check(map[string]domain.Access{
		"serving.ondabberz.link": public,
		"change.ondabberz.link":  shut,
		"plain.ondabberz.link":   public,
	})

	// Locked by the owner, the site is locked.
	installation.Access = shut
	if err := s.UpdateInstallation(ctx, installation); err != nil {
		t.Fatalf("UpdateInstallation: %v", err)
	}
	check(map[string]domain.Access{
		"serving.ondabberz.link": installation.Access,
		"change.ondabberz.link":  shut,
		"plain.ondabberz.link":   public,
	})

	// One installed before policies existed has none stored, and stays as
	// open as it was rather than starting to demand sign-ins.
	if _, err := s.db.ExecContext(ctx, "UPDATE installations SET access = '' WHERE id = ?", installation.ID); err != nil {
		t.Fatal(err)
	}
	again, err := s.GetInstallation(ctx, installation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Access.Mode != domain.AccessPublic {
		t.Errorf("an installation from before policies reads back as %q, want public", again.Access.Mode)
	}

	// A policy the edge could not enforce as written is never stored.
	installation.Access = domain.Access{Mode: domain.AccessHybrid}
	if err := s.UpdateInstallation(ctx, installation); err == nil {
		t.Error("a hybrid policy with no public paths was stored")
	}
}
