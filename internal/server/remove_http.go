package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

func (u *ui) removeRepository(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rp, err := u.c.Catalog.Repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.FormValue("confirm") != rp.Name {
		redirectNotice(w, r, "/repositories", "Type the repository name to confirm removing it.")
		return
	}
	if err := u.c.RemoveRepository(r.Context(), id); err != nil {
		redirectNotice(w, r, "/repositories", "Not removed: "+err.Error())
		return
	}
	redirectNotice(w, r, "/repositories", "Removed "+rp.Name+" from the server. Its data in storage was not touched.")
}

func (u *ui) wipeRepository(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rp, err := u.c.Catalog.Repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.FormValue("confirm") != rp.Name {
		redirectNotice(w, r, "/repositories", "Type the repository name to confirm wiping it.")
		return
	}
	if r.FormValue("agent") == "" {
		redirectNotice(w, r, "/repositories", "Pick the agent that will delete the data.")
		return
	}
	if err := u.c.StartWipe(r.Context(), id, r.FormValue("agent")); err != nil {
		redirectNotice(w, r, "/repositories", "Not wiped: "+err.Error())
		return
	}
	redirectNotice(w, r, "/repositories", "Wiping "+rp.Name+" in the background. It is removed from the list when the data is gone.")
}

func (u *ui) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	if _, err := u.c.Catalog.Job(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	if r.FormValue("confirm") != shortID(sid) {
		redirectNotice(w, r, "/jobs/"+id, "Type the snapshot ID shown in the box to confirm deleting it.")
		return
	}
	if err := u.c.DeleteSnapshot(r.Context(), id, sid); err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		redirectNotice(w, r, "/jobs/"+id, "Not deleted: "+err.Error())
		return
	}
	redirectNotice(w, r, "/jobs/"+id, "Deleted snapshot "+shortID(sid)+". Its space is freed by a later maintenance run.")
}

// maintainRepository runs a full maintenance cycle in the background: it can
// take a long time on a big repository.
func (u *ui) maintainRepository(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rp, err := u.c.Catalog.Repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if rp.InitializedAt.IsZero() {
		redirectNotice(w, r, "/repositories", rp.Name+" is not initialized yet.")
		return
	}
	if u.c.IsRunning(repoMaintRunKey(id)) || u.c.IsRunning(repoWipeKey(id)) {
		redirectNotice(w, r, "/repositories", "Maintenance of "+rp.Name+" is already running, or the repository is being wiped.")
		return
	}
	go func() {
		if _, err := u.c.MaintainRepository(context.Background(), id); err != nil && !errors.Is(err, ErrRunning) {
			slog.Error("manual maintenance", "repo", id, "err", err)
		}
	}()
	redirectNotice(w, r, "/repositories", "Maintenance of "+rp.Name+" started. Data deleted recently is released only after Kopia's safety delays (about two days).")
}

// setMaintainer hands a repository's maintenance to the server, or back to an
// agent. It is typed-name confirmed because it changes the Kopia owner.
func (u *ui) setMaintainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	back := "/repositories"
	if r.FormValue("from") == "agents" {
		back = "/" // the form on a refused agent delete
	}
	rp, err := u.c.Catalog.Repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.FormValue("confirm") != rp.Name {
		redirectNotice(w, r, back, "Type the repository name to confirm changing who maintains it.")
		return
	}
	who := r.FormValue("who")
	warnings, err := u.c.SetMaintainer(r.Context(), id, who, r.FormValue("agent"), r.FormValue("server_path"))
	if err != nil {
		redirectNotice(w, r, back, "Not changed: "+err.Error())
		return
	}
	msg := "Maintenance of " + rp.Name + " is now run by the " + who + "."
	if len(warnings) > 0 {
		msg += " Warning: " + strings.Join(warnings, " ")
	}
	redirectNotice(w, r, back, msg)
}

// assignAgent tests that an agent can open the repository, then lets it use it.
func (u *ui) assignAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rp, err := u.c.Catalog.Repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	warnings, err := u.c.AssignAgent(r.Context(), id, r.FormValue("agent"), r.FormValue("key_file"))
	if err != nil {
		redirectNotice(w, r, "/repositories", "Not assigned: "+err.Error())
		return
	}
	msg := "The agent opened " + rp.Name + " and can use it now."
	if len(warnings) > 0 {
		msg += " Warning: " + strings.Join(warnings, " ")
	}
	redirectNotice(w, r, "/repositories", msg)
}

// defaultRepository makes a repository the one new jobs on an agent start with.
func (u *ui) defaultRepository(w http.ResponseWriter, r *http.Request) {
	rp, err := u.c.Catalog.Repository(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := u.c.SetDefaultRepository(r.Context(), rp.ID, r.PathValue("agent")); err != nil {
		redirectNotice(w, r, "/repositories", "Not changed: "+err.Error())
		return
	}
	redirectNotice(w, r, "/repositories", rp.Name+" is now what new jobs on that agent start with.")
}

func (u *ui) unassignAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rp, err := u.c.Catalog.Repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := u.c.UnassignAgent(r.Context(), id, r.PathValue("agent")); err != nil {
		redirectNotice(w, r, "/repositories", "Not removed: "+err.Error())
		return
	}
	redirectNotice(w, r, "/repositories", "The agent can no longer get new jobs on "+rp.Name+".")
}
