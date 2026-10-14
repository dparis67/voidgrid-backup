package repocfg

import "testing"

func TestForAgentResolvesTheKey(t *testing.T) {
	c := Config{Kind: KindSFTP, SFTP: &SFTP{Host: "h", Port: 23, User: "u", Path: "p", KeyFile: "/keys/default",
		KeyFiles: map[string]string{"a1": "/keys/a1"}}}
	if got := c.ForAgent("a1").SFTP.KeyFile; got != "/keys/a1" {
		t.Errorf("a1 key = %q", got)
	}
	if got := c.ForAgent("a2").SFTP.KeyFile; got != "/keys/default" {
		t.Errorf("a2 key = %q, want the default", got)
	}
	if c.ForAgent("a1").SFTP.KeyFiles != nil {
		t.Error("an agent was sent the table of every agent's key")
	}
	if c.SFTP.KeyFile != "/keys/default" || c.SFTP.KeyFiles == nil {
		t.Error("ForAgent changed the original")
	}
	// A config that never used the table fingerprints as before.
	plain := Config{Kind: KindSFTP, SFTP: &SFTP{Host: "h", Port: 23, User: "u", Path: "p", KeyFile: "/keys/default"}}
	if plain.Fingerprint() != c.Fingerprint() {
		t.Error("the key table changed the fingerprint of the default config")
	}
	// Only per-agent keys is a valid repository.
	onlyTable := Config{Kind: KindSFTP, SFTP: &SFTP{Host: "h", Port: 23, User: "u", Path: "p", KeyFiles: map[string]string{"a1": "/k"}}}
	if err := onlyTable.Validate(); err != nil {
		t.Errorf("per-agent keys only: %v", err)
	}
}
