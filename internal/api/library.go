package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/install"
	"github.com/dabbers/devex/internal/library"
	"github.com/dabbers/devex/internal/store"
)

// The library and settings handlers.
//
// An upload is the file's bytes as the request body, with its path in the
// query, rather than a multipart form: one file, one request, nothing to parse
// but the file. Whatever arrives is judged by library.Admit before anything is
// kept, and the refusal says why in words a person can act on.

func (s *Server) listAccountLibrary(w http.ResponseWriter, r *http.Request) {
	if s.library == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no library")
		return
	}
	files, err := s.store.ListLibraryFiles(r.Context(), s.userID(r), "")
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": nonNil(files)})
}

func (s *Server) listInstallationLibrary(w http.ResponseWriter, r *http.Request) {
	if s.library == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no library")
		return
	}
	installation := scopedInstallation(r)
	own, err := s.store.ListLibraryFiles(r.Context(), installation.UserID, installation.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	shared, err := s.store.ListLibraryFiles(r.Context(), installation.UserID, "")
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": nonNil(own), "account": nonNil(shared)})
}

func (s *Server) putAccountLibraryFile(w http.ResponseWriter, r *http.Request) {
	s.putLibraryFile(w, r, "")
}

func (s *Server) putInstallationLibraryFile(w http.ResponseWriter, r *http.Request) {
	s.putLibraryFile(w, r, scopedInstallation(r).ID)
}

func (s *Server) putLibraryFile(w http.ResponseWriter, r *http.Request, scope string) {
	if s.library == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no library")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, library.MaxFileBytes+1))
	if err != nil {
		writeStatus(w, http.StatusRequestEntityTooLarge, "a file may be at most 10 MiB")
		return
	}
	f, err := s.library.Put(r.Context(), s.userID(r), scope, r.URL.Query().Get("path"), data, "person:"+s.userID(r))
	if errors.Is(err, library.ErrRefused) {
		writeStatus(w, http.StatusUnprocessableEntity, strings.TrimPrefix(err.Error(), library.ErrRefused.Error()+": "))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": f})
}

// libraryFileContent returns a file's bytes to its owner. Never as a page:
// pictures are pictures, and anything else is a download, sandboxed and
// unsniffable even if someone opens it directly.
func (s *Server) libraryFileContent(w http.ResponseWriter, r *http.Request) {
	if s.library == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no library")
		return
	}
	f := scopedFile(r)
	data, err := s.library.Read(f)
	if err != nil {
		writeError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-store")
	if f.Kind != string(library.KindImage) {
		name := f.Path[strings.LastIndexByte(f.Path, '/')+1:]
		h.Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
	}
	_, _ = w.Write(data)
}

func (s *Server) deleteLibraryFile(w http.ResponseWriter, r *http.Request) {
	if s.library == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment has no library")
		return
	}
	f := scopedFile(r)
	if err := s.library.Delete(r.Context(), f.UserID, f.ID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": f.ID})
}

func (s *Server) getInstallationSettings(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment cannot manage installations")
		return
	}
	view, err := s.installer.Settings(r.Context(), scopedInstallation(r))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) putInstallationSettings(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeStatus(w, http.StatusNotImplemented, "this deployment cannot manage installations")
		return
	}
	var body struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// Decoded one by one so null survives as "clear this one", which a
	// map[string]any cannot tell apart from absent.
	values := map[string]any{}
	for path, raw := range body.Values {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			writeStatus(w, http.StatusBadRequest, "unreadable value for "+path)
			return
		}
		values[path] = v
	}
	view, err := s.installer.SetSettings(r.Context(), scopedInstallation(r), "person:"+s.userID(r), values)
	if errors.Is(err, install.ErrInvalid) {
		writeStatus(w, http.StatusUnprocessableEntity, strings.TrimPrefix(err.Error(), install.ErrInvalid.Error()+": "))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func nonNil(files []*store.LibraryFile) []*store.LibraryFile {
	if files == nil {
		return []*store.LibraryFile{}
	}
	return files
}

// prepareLibrary gives a workspace working on an installation a fresh copy of
// the library and the app's settings, returning the installation, or nil for a
// workspace that is not one. A failure is logged and the turn goes ahead: an
// agent without the library can still do what was asked.
func (s *Server) prepareLibrary(ctx context.Context, ws *domain.Workspace) *domain.Installation {
	if s.library == nil || s.installer == nil || s.workspaces == nil || ws.InstanceID == "" {
		return nil
	}
	installation, err := s.installer.InstallationFor(ctx, ws)
	if err != nil || installation == nil {
		return nil
	}
	if err := s.library.Sync(ctx, s.workspaces, ws.InstanceID, installation.UserID, installation.ID); err != nil {
		s.logger.Warn("could not copy the library into a workspace", "workspace", ws.ID, "error", err)
	}
	s.installer.RefreshSettings(ctx, ws)
	return installation
}

// collectLibrary takes what the agent left for the library, gives the app its
// settings again in case the agent changed what it declares, and says what
// happened in words for the reply.
func (s *Server) collectLibrary(ctx context.Context, ws *domain.Workspace, installation *domain.Installation) string {
	defer s.installer.RefreshSettings(ctx, ws)
	got, err := s.library.Collect(ctx, s.workspaces, ws.InstanceID, installation.UserID, installation.ID, "agent:"+ws.ID)
	if err != nil {
		s.logger.Warn("could not collect the library outbox", "workspace", ws.ID, "error", err)
		return "(Files left for the library could not be read: " + err.Error() + ")"
	}
	var lines []string
	for _, f := range got.Saved {
		where := "the account library"
		if f.Scope != "" {
			where = "this app's library"
		}
		lines = append(lines, "Saved "+f.Path+" to "+where+".")
	}
	for _, why := range got.Refused {
		lines = append(lines, "Not saved: "+why)
	}
	return strings.Join(lines, "\n")
}
