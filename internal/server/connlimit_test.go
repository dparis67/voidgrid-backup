package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

func sftpCfg(host, path string) repocfg.Config {
	return repocfg.Config{Kind: repocfg.KindSFTP, SFTP: &repocfg.SFTP{Host: host, Port: 23, User: "u1", Path: path, KeyFile: "/k"}}
}

func TestConnSlotsHoldBackAtTheLimit(t *testing.T) {
	var s connSlots
	ctx := context.Background()
	r1, err := s.acquire(ctx, "h:23", 2)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := s.acquire(ctx, "h:23", 2)
	if other, err := s.acquire(ctx, "other:23", 2); err != nil { // another host has its own count
		t.Fatal(err)
	} else {
		other()
	}
	var got atomic.Bool
	done := make(chan struct{})
	go func() {
		r3, err := s.acquire(ctx, "h:23", 2)
		if err == nil {
			got.Store(true)
			r3()
		}
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	if got.Load() {
		t.Fatal("a third operation ran at a limit of 2")
	}
	r1()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("giving a slot back did not let the waiter in")
	}
	r2()

	cctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	a, _ := s.acquire(ctx, "x", 1)
	if _, err := s.acquire(cctx, "x", 1); err == nil {
		t.Fatal("a waiter must give up when its context ends")
	}
	a()
	if rel, err := s.acquire(ctx, "x", 0); err != nil { // no limit never waits
		t.Fatal(err)
	} else {
		rel()
	}
}

func TestConnLimitDefaultsAndSetting(t *testing.T) {
	ctx := context.Background()
	c, _, _ := setupJob(t)
	hz := sftpCfg("u677522.your-storagebox.de", "/a")
	other := sftpCfg("backup.example.org", "/a")
	if _, l := c.connLimit(ctx, hz); l != 10 {
		t.Fatalf("hetzner default = %d, want 10", l)
	}
	if _, l := c.connLimit(ctx, other); l != 0 {
		t.Fatalf("other host default = %d, want no limit", l)
	}
	if err := c.SetSFTPCap(ctx, CapFixed, 4); err != nil {
		t.Fatal(err)
	}
	if _, l := c.connLimit(ctx, other); l != 4 {
		t.Fatalf("fixed = %d, want 4", l)
	}
	if err := c.SetSFTPCap(ctx, CapOff, 0); err != nil {
		t.Fatal(err)
	}
	if _, l := c.connLimit(ctx, hz); l != 0 {
		t.Fatalf("off must lift the Hetzner limit, got %d", l)
	}
	if err := c.SetSFTPCap(ctx, CapFixed, 0); err == nil {
		t.Fatal("a fixed limit of 0 must be refused")
	}
	if err := c.SetSFTPCap(ctx, CapAuto, 0); err != nil {
		t.Fatal(err)
	}
	if _, l := c.connLimit(ctx, hz); l != 10 {
		t.Fatalf("back to automatic = %d", l)
	}
	if _, l := c.connLimit(ctx, repocfg.Config{Kind: repocfg.KindS3, S3: &repocfg.S3{}}); l != 0 {
		t.Fatal("only SFTP is limited")
	}
}

func TestCheckLocationFree(t *testing.T) {
	ctx := context.Background()
	c, _, _ := setupJob(t)
	cfg := sftpCfg("Box.Example.org", "/backups/a/")
	if err := c.Catalog.AddRepository(ctx, catalog.Repository{ID: "r-a", Name: "A", Config: cfg, Password: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := c.checkLocationFree(ctx, sftpCfg("box.example.org", "/backups/a"), ""); err == nil || !strings.Contains(err.Error(), `"A"`) {
		t.Fatalf("the same place under another spelling must be refused, got %v", err)
	}
	if err := c.checkLocationFree(ctx, sftpCfg("box.example.org", "/backups/a"), "r-a"); err != nil {
		t.Fatalf("a repository may keep its own place when edited: %v", err)
	}
	if err := c.checkLocationFree(ctx, sftpCfg("box.example.org", "/backups/b"), ""); err != nil {
		t.Fatalf("a different path is fine: %v", err)
	}
	s3a := repocfg.Config{Kind: repocfg.KindS3, S3: &repocfg.S3{Endpoint: "s3.x.com", Bucket: "b", Prefix: "p/"}}
	s3b := repocfg.Config{Kind: repocfg.KindS3, S3: &repocfg.S3{Endpoint: "S3.X.com", Bucket: "b", Prefix: "/p"}}
	if s3a.Identity() != s3b.Identity() {
		t.Fatalf("s3 identities differ: %q %q", s3a.Identity(), s3b.Identity())
	}
}

func TestSFTPCapForm(t *testing.T) {
	ctx := context.Background()
	c, _, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	post := func(form url.Values) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/settings/sftp-cap", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		tidy.Close(resp.Body)
	}
	post(url.Values{"mode": {"fixed"}, "n": {"0"}})
	if s := c.SFTPCap(ctx); s.Mode != CapAuto {
		t.Fatalf("a bad number was saved: %+v", s)
	}
	post(url.Values{"mode": {"fixed"}, "n": {"6"}})
	if s := c.SFTPCap(ctx); s.Mode != CapFixed || s.N != 6 {
		t.Fatalf("saved: %+v", s)
	}
	post(url.Values{"mode": {"off"}})
	if s := c.SFTPCap(ctx); s.Mode != CapOff {
		t.Fatalf("saved: %+v", s)
	}
	resp, err := client.Get(srv.URL + "/repositories")
	if err != nil {
		t.Fatal(err)
	}
	defer tidy.Close(resp.Body)
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), "SFTP connection limit") {
		t.Fatal("the Repositories page does not show the connection limit form")
	}
}
