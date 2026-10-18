package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

type repoEditPage struct {
	base
	Repo catalog.Repository
	Form url.Values
}

// editRepositoryForm shows the settings of a repository that can be changed.
func (u *ui) editRepositoryForm(w http.ResponseWriter, r *http.Request) {
	rp, err := u.c.Catalog.Repository(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	u.render(w, http.StatusOK, "repoedit", repoEditPage{base: u.newBase("Edit "+rp.Name, "repos", r), Repo: rp, Form: url.Values{}})
}

// editRepository applies an edit: the data is moved first when the path
// changed, and nothing is saved unless the repository opens afterwards.
func (u *ui) editRepository(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	rp, err := u.c.Catalog.Repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f := r.PostForm
	in := RepoEdit{
		SFTPPath: f.Get("sftp_path"), SFTPKeyFile: f.Get("sftp_key_file"), SFTPPassword: f.Get("sftp_password"),
		SFTPKnownHosts: f.Get("sftp_known_hosts"), S3AccessKey: f.Get("s3_access_key"), S3Secret: f.Get("s3_secret"),
		FSPath: f.Get("fs_path"),
	}
	ctx, cancel := context.WithTimeout(r.Context(), editTimeout+time.Minute)
	defer cancel()
	warnings, err := u.c.EditRepository(ctx, id, in)
	if err != nil {
		// Secrets are not echoed back.
		for _, k := range []string{"sftp_password", "s3_secret"} {
			f.Del(k)
		}
		p := repoEditPage{base: u.newBase("Edit "+rp.Name, "repos", r), Repo: rp, Form: f}
		p.Error = "Not changed: " + err.Error()
		u.render(w, http.StatusBadRequest, "repoedit", p)
		return
	}
	msg := "Saved the settings of " + rp.Name + "."
	if len(warnings) > 0 {
		msg += " Warning: " + strings.Join(warnings, " ")
	}
	redirectNotice(w, r, "/repositories", msg)
}
