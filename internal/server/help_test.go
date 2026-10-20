package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

var hrefRe = regexp.MustCompile(`href="([^"]+)"`)

// Every link in every doc must lead somewhere: an anchor on the same page, or a
// page and anchor of another doc. A heading renamed in the docs fails this.
func TestHelpDocsLinksResolve(t *testing.T) {
	all, err := helpDocs()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(helpPages) {
		t.Fatalf("rendered %d docs, want %d", len(all), len(helpPages))
	}
	checked := 0
	for name, d := range all {
		for _, m := range hrefRe.FindAllStringSubmatch(string(d.HTML), -1) {
			href := m[1]
			checked++
			switch {
			case strings.Contains(href, "://"):
			case strings.HasPrefix(href, "#"):
				if !d.Anchors[href[1:]] {
					t.Errorf("%s: no heading for %s", name, href)
				}
			case strings.HasPrefix(href, "/help/"):
				page, frag, _ := strings.Cut(strings.TrimPrefix(href, "/help/"), "#")
				target, ok := all[page]
				if !ok {
					t.Errorf("%s: link to missing page %s", name, href)
				} else if frag != "" && !target.Anchors[frag] {
					t.Errorf("%s: no heading %q in %s", name, frag, page)
				}
			default:
				t.Errorf("%s: link %q goes outside the docs and the web", name, href)
			}
		}
	}
}

func TestHelpLinkCheckSawLinks(t *testing.T) {
	all, _ := helpDocs()
	n := 0
	for _, d := range all {
		n += strings.Count(string(d.HTML), `href="#`) + strings.Count(string(d.HTML), `href="/help/`)
	}
	if n < 10 {
		t.Fatalf("only %d internal links found in the docs; the link check proves little", n)
	}
}

func TestHelpLinkRewrite(t *testing.T) {
	for in, want := range map[string]string{
		"agent.md":                 "/help/agent",
		"server.md#maintenance":    "/help/server#maintenance",
		"#local":                   "#local",
		"https://example.org/a.md": "https://example.org/a.md",
		"../README.md":             "../README.md",
	} {
		if got := helpLink(in); got != want {
			t.Errorf("helpLink(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHelpDropsRawHTMLAndRendersTables(t *testing.T) {
	d, err := renderDoc([]byte("# T\n\n<script>alert(1)</script>\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n## Same\n\n## Same\n"))
	if err != nil {
		t.Fatal(err)
	}
	h := string(d.HTML)
	if strings.Contains(h, "<script>") {
		t.Fatalf("raw HTML was passed through: %s", h)
	}
	if !strings.Contains(h, "<table>") {
		t.Fatalf("no table: %s", h)
	}
	if !d.Anchors["same"] || !d.Anchors["same-1"] {
		t.Fatalf("repeated headings need distinct ids, got %v", d.Anchors)
	}
}

func TestHelpPages(t *testing.T) {
	c, _, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer tidy.Close(resp.Body)
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/help"); code != http.StatusOK || !strings.Contains(body, `href="/help/server"`) || !strings.Contains(body, `href="/help"`) {
		t.Fatalf("index: %d", code)
	}
	if code, body := get("/help/server"); code != http.StatusOK || !strings.Contains(body, `class="doc"`) || !strings.Contains(body, "<table>") {
		t.Fatalf("server doc: %d", code)
	}
	if code, _ := get("/help/nope"); code != http.StatusNotFound {
		t.Fatalf("unknown doc: %d, want 404", code)
	}
}
