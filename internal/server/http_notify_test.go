package server

import (
	"net/url"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/notify"
)

func TestNotifyConfigFromFormKeepsSecretsWhenBlank(t *testing.T) {
	existing := notify.Config{
		Discord:  &notify.DiscordConfig{Enabled: true, WebhookURL: "https://discord.example/old"},
		Notifarr: &notify.NotifarrConfig{Enabled: true, APIKey: "old-key", ChannelID: 111},
		Email:    &notify.EmailConfig{Enabled: true, Host: "smtp.example", Port: 587, Password: "old-pw", From: "a@x", To: []string{"b@x"}},
	}
	f := url.Values{
		"discord_enabled":  {"on"},                                 // webhook left blank
		"notifarr_enabled": {"on"}, "notifarr_channel_id": {"222"}, // api key left blank, channel changed
		"email_enabled": {"on"}, "email_host": {"smtp.example"}, "email_port": {"587"},
		"email_from": {"a@x"}, "email_to": {"b@x"}, // password left blank
	}
	cfg, err := notifyConfigFromForm(f, existing)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discord.WebhookURL != "https://discord.example/old" {
		t.Fatalf("discord webhook not kept: %+v", cfg.Discord)
	}
	if cfg.Notifarr.APIKey != "old-key" || cfg.Notifarr.ChannelID != 222 {
		t.Fatalf("notifarr not merged correctly: %+v", cfg.Notifarr)
	}
	if cfg.Email.Password != "old-pw" {
		t.Fatalf("email password not kept: %+v", cfg.Email)
	}
}

func TestNotifyConfigFromFormRequiresSecretOnFirstSave(t *testing.T) {
	f := url.Values{"discord_enabled": {"on"}} // no webhook, no existing config
	if _, err := notifyConfigFromForm(f, notify.Config{}); err == nil {
		t.Fatal("discord enabled with no webhook and nothing stored: accepted")
	}
}

// Switching a channel off keeps what was saved for it, so it can be switched
// back on without retyping its secret, and it stops being sent to.
func TestNotifyConfigFromFormDisabledChannelIsKeptOff(t *testing.T) {
	existing := notify.Config{
		Discord: &notify.DiscordConfig{Enabled: true, WebhookURL: "https://discord.example/old"},
		Email:   &notify.EmailConfig{Enabled: true, Host: "smtp.example", Port: 587, From: "a@x", To: []string{"b@x"}, Password: "pw"},
	}
	cfg, err := notifyConfigFromForm(url.Values{}, existing)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discord == nil || cfg.Discord.Enabled || cfg.Discord.WebhookURL != "https://discord.example/old" {
		t.Fatalf("an unchecked channel should be kept, off, with its secret: %+v", cfg.Discord)
	}
	if cfg.Email == nil || cfg.Email.Enabled || cfg.Email.Password != "pw" {
		t.Fatalf("an unchecked email channel should be kept, off, with its password: %+v", cfg.Email)
	}
	if cfg.Enabled() {
		t.Fatal("no channel should be on")
	}

	// Edits made while it is off are saved when they are complete; switching it
	// back on needs no secret retyped.
	f := url.Values{"email_host": {"smtp2.example"}, "email_port": {"465"}, "email_from": {"a@x"}, "email_to": {"c@x"}}
	saved, err := notifyConfigFromForm(f, existing)
	if err != nil || saved.Email.Enabled || saved.Email.Host != "smtp2.example" || saved.Email.Password != "pw" {
		t.Fatalf("edit while off: %+v %v", saved.Email, err)
	}
	f.Set("email_enabled", "on")
	again, err := notifyConfigFromForm(f, saved)
	if err != nil || !again.Email.Enabled || again.Email.Password != "pw" {
		t.Fatalf("back on: %+v %v", again.Email, err)
	}

	// A channel never configured stays absent.
	cfg, err = notifyConfigFromForm(url.Values{}, notify.Config{})
	if err != nil || cfg.Discord != nil || cfg.Notifarr != nil || cfg.Email != nil {
		t.Fatalf("empty form: %+v %v", cfg, err)
	}
}

func TestNotifyConfigFromFormValidatesEmailFields(t *testing.T) {
	base := url.Values{"email_enabled": {"on"}, "email_host": {"smtp.example"}, "email_port": {"587"}, "email_from": {"a@x"}, "email_to": {"b@x"}}
	if _, err := notifyConfigFromForm(url.Values{"email_enabled": {"on"}}, notify.Config{}); err == nil {
		t.Fatal("email with no host/from/to accepted")
	}
	if _, err := notifyConfigFromForm(base, notify.Config{}); err != nil {
		t.Fatalf("valid email config rejected: %v", err)
	}
	bad := url.Values{}
	for k, v := range base {
		bad[k] = v
	}
	bad.Set("email_port", "not-a-number")
	if _, err := notifyConfigFromForm(bad, notify.Config{}); err == nil {
		t.Fatal("non-numeric port accepted")
	}
}
