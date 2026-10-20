package server

import (
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"

	"github.com/voidgrid/voidgrid-backup/docs"
)

// helpPages are the docs the Help pages show, in the order they are listed.
// The name is the file's name under docs/ without ".md".
var helpPages = []struct{ Name, Label, About string }{
	{"server", "Server", "Setting up and using the server and its web UI: agents, repositories, jobs, maintenance, restores, schedule, notifications, authentication."},
	{"agent", "Agent", "Running an agent on a host: configuration, SFTP keys, Docker stacks, VMs, and what happens when the network fails."},
	{"recovery", "Recovery", "Restoring straight from storage with voidgrid-backup-recover when the server and its catalog are gone."},
	{"testing", "Testing", "What has been tested, against what, and what has not."},
}

// helpDoc is one rendered documentation page.
type helpDoc struct {
	Name, Label, About string
	HTML               []byte          // rendered; written to the page as it is, never through html/template
	Anchors            map[string]bool // the heading ids on the page
}

var (
	helpOnce sync.Once
	helpMap  map[string]*helpDoc
	helpErr  error
)

// helpDocs renders the embedded docs once.
func helpDocs() (map[string]*helpDoc, error) {
	helpOnce.Do(func() {
		helpMap = map[string]*helpDoc{}
		for _, p := range helpPages {
			src, err := fs.ReadFile(docs.FS, p.Name+".md")
			if err != nil {
				helpErr = err
				return
			}
			d, err := renderDoc(src)
			if err != nil {
				helpErr = fmt.Errorf("render %s: %w", p.Name, err)
				return
			}
			d.Name, d.Label, d.About = p.Name, p.Label, p.About
			helpMap[p.Name] = d
		}
	})
	return helpMap, helpErr
}

// renderDoc turns one Markdown page into HTML. Raw HTML in the source is not
// passed through. Headings get GitHub-style ids, so the anchors the docs link
// to work here as they do on GitHub, and links to the other docs pages point at
// the Help pages.
func renderDoc(src []byte) (*helpDoc, error) {
	ids := &headingIDs{used: map[string]bool{}}
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
			parser.WithASTTransformers(util.Prioritized(docLinks{}, 100)),
		),
	)
	var buf bytes.Buffer // our own embedded docs, and goldmark drops raw HTML, so the result is safe to show as HTML
	if err := md.Convert(src, &buf, parser.WithContext(parser.NewContext(parser.WithIDs(ids)))); err != nil {
		return nil, err
	}
	return &helpDoc{HTML: buf.Bytes(), Anchors: ids.used}, nil
}

// headingIDs makes heading ids the way GitHub does: lower case, spaces to
// hyphens, punctuation dropped, a numeric suffix for a repeat.
type headingIDs struct{ used map[string]bool }

func (h *headingIDs) Generate(value []byte, _ ast.NodeKind) []byte {
	var b strings.Builder
	for _, r := range strings.ToLower(string(value)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteByte('-')
		}
	}
	base := b.String()
	if base == "" {
		base = "section"
	}
	id := base
	for n := 1; h.used[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	h.used[id] = true
	return []byte(id)
}

func (h *headingIDs) Put(value []byte) { h.used[string(value)] = true }

// docLinks points a link to another docs page (agent.md#keys) at its Help page
// (/help/agent#keys).
type docLinks struct{}

func (docLinks) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if l, ok := n.(*ast.Link); ok && entering {
			l.Destination = []byte(helpLink(string(l.Destination)))
		}
		return ast.WalkContinue, nil
	})
}

// helpLink is the target a docs link has in the UI. Only a link to another
// docs page changes.
func helpLink(dest string) string {
	if strings.Contains(dest, "://") || strings.HasPrefix(dest, "#") {
		return dest
	}
	file, frag, _ := strings.Cut(dest, "#")
	name, ok := strings.CutSuffix(file, ".md")
	if !ok || strings.Contains(name, "/") {
		return dest
	}
	out := "/help/" + url.PathEscape(name)
	if frag != "" {
		out += "#" + frag
	}
	return out
}

type helpPage struct {
	base
	Pages []*helpDoc // the index
	Doc   *helpDoc   // the page, nil on the index
}

func (u *ui) helpIndex(w http.ResponseWriter, r *http.Request) {
	all, err := helpDocs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p := helpPage{base: u.newBase("Help", "help", r)}
	for _, hp := range helpPages {
		p.Pages = append(p.Pages, all[hp.Name])
	}
	u.render(w, http.StatusOK, "help", p)
}

func (u *ui) helpDoc(w http.ResponseWriter, r *http.Request) {
	all, err := helpDocs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d, ok := all[r.PathValue("name")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	p := helpPage{base: u.newBase(d.Label+" docs", "help", r), Doc: d}
	for _, hp := range helpPages {
		p.Pages = append(p.Pages, all[hp.Name])
	}
	// The page's frame is a template; the document goes between its halves as
	// the bytes goldmark made, which html/template would only escape.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var page bytes.Buffer
	for _, step := range []func() error{
		func() error { return u.tmpl.ExecuteTemplate(&page, "help-top", p) },
		func() error { _, err := page.Write(d.HTML); return err },
		func() error { return u.tmpl.ExecuteTemplate(&page, "help-bottom", p) },
	} {
		if err := step(); err != nil {
			slog.Error("render", "page", "help", "err", err)
			http.Error(w, "could not render the page", http.StatusInternalServerError)
			return
		}
	}
	_, _ = w.Write(page.Bytes())
}
