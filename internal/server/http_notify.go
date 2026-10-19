package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/notify"
)

type notificationsPage struct {
	base
	Cfg         notify.Config
	Form        url.Values
	TestResults []notify.Result
	TestError   string
}

func (u *ui) notifications(w http.ResponseWriter, r *http.Request) {
	cfg, err := u.c.NotifyConfig(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, http.StatusOK, "notifications", notificationsPage{
		base: u.newBase("Notifications", "notifications", r), Cfg: cfg, Form: formFromConfig(cfg),
	})
}

// formFromConfig seeds the form with everything except secrets, which stay
// blank: the "leave blank to keep the current value" convention below means
// a blank secret field is never actually "no secret", except on the very
// first save.
func formFromConfig(cfg notify.Config) url.Values {
	f := url.Values{"url": {cfg.URL}}
	if cfg.Discord != nil {
		if cfg.Discord.Enabled {
			f.Set("discord_enabled", "on")
		}
	}
	if cfg.Notifarr != nil {
		if cfg.Notifarr.Enabled {
			f.Set("notifarr_enabled", "on")
		}
		f.Set("notifarr_channel_id", formatInt64(cfg.Notifarr.ChannelID))
		f.Set("notifarr_color", cfg.Notifarr.Color)
	} else {
		f.Set("notifarr_color", "b3261e")
	}
	if cfg.Email != nil {
		if cfg.Email.Enabled {
			f.Set("email_enabled", "on")
		}
		f.Set("email_host", cfg.Email.Host)
		f.Set("email_port", strconv.Itoa(cfg.Email.Port))
		if cfg.Email.ImplicitTLS {
			f.Set("email_implicit_tls", "on")
		}
		f.Set("email_username", cfg.Email.Username)
		f.Set("email_from", cfg.Email.From)
		f.Set("email_to", strings.Join(cfg.Email.To, ", "))
	} else {
		f.Set("email_port", "587")
	}
	return f
}

func formatInt64(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func (u *ui) saveNotifications(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	existing, err := u.c.NotifyConfig(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cfg, err := notifyConfigFromForm(r.PostForm, existing)
	if err != nil {
		p := notificationsPage{base: u.newBase("Notifications", "notifications", r), Cfg: existing, Form: r.PostForm}
		p.Error = err.Error()
		u.render(w, http.StatusBadRequest, "notifications", p)
		return
	}
	if err := u.c.SetNotifyConfig(ctx, cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectNotice(w, r, "/notifications", "Notification settings saved.")
}

// notifyConfigFromForm builds a notify.Config from submitted form values. A
// blank secret field (webhook URL, API key, SMTP password) keeps whatever
// was already saved for that channel, so re-saving other fields never
// requires retyping it; the way to actually change one is to type a new
// value.
//
// Unchecking "enabled" turns a channel off without forgetting it: its settings
// and secrets are kept (from the form when they are complete, else as already
// saved) and only Enabled is cleared, so it can be switched back on, or left
// off while another channel is tested. Only an enabled channel must be valid.
func notifyConfigFromForm(f url.Values, existing notify.Config) (notify.Config, error) {
	cfg := notify.Config{URL: strings.TrimSpace(f.Get("url"))}

	d, err := discordFromForm(f, existing.Discord)
	switch {
	case f.Get("discord_enabled") != "":
		if err != nil {
			return cfg, err
		}
		cfg.Discord = d
	case err == nil:
		d.Enabled = false
		cfg.Discord = d
	case existing.Discord != nil:
		kept := *existing.Discord
		kept.Enabled = false
		cfg.Discord = &kept
	}

	n, err := notifarrFromForm(f, existing.Notifarr)
	switch {
	case f.Get("notifarr_enabled") != "":
		if err != nil {
			return cfg, err
		}
		cfg.Notifarr = n
	case err == nil:
		n.Enabled = false
		cfg.Notifarr = n
	case existing.Notifarr != nil:
		kept := *existing.Notifarr
		kept.Enabled = false
		cfg.Notifarr = &kept
	}

	e, err := emailFromForm(f, existing.Email)
	switch {
	case f.Get("email_enabled") != "":
		if err != nil {
			return cfg, err
		}
		cfg.Email = e
	case err == nil:
		e.Enabled = false
		cfg.Email = e
	case existing.Email != nil:
		kept := *existing.Email
		kept.Enabled = false
		cfg.Email = &kept
	}
	return cfg, nil
}

func discordFromForm(f url.Values, existing *notify.DiscordConfig) (*notify.DiscordConfig, error) {
	webhook := strings.TrimSpace(f.Get("discord_webhook_url"))
	if webhook == "" && existing != nil {
		webhook = existing.WebhookURL
	}
	if webhook == "" {
		return nil, errors.New("discord: webhook URL is required")
	}
	return &notify.DiscordConfig{Enabled: true, WebhookURL: webhook}, nil
}

func notifarrFromForm(f url.Values, existing *notify.NotifarrConfig) (*notify.NotifarrConfig, error) {
	apiKey := strings.TrimSpace(f.Get("notifarr_api_key"))
	if apiKey == "" && existing != nil {
		apiKey = existing.APIKey
	}
	if apiKey == "" {
		return nil, errors.New("notifarr: API key is required")
	}
	channelID, err := strconv.ParseInt(strings.TrimSpace(f.Get("notifarr_channel_id")), 10, 64)
	if err != nil || channelID == 0 {
		return nil, errors.New("notifarr: a numeric Discord channel ID is required")
	}
	return &notify.NotifarrConfig{
		Enabled: true, APIKey: apiKey, ChannelID: channelID,
		Color: strings.TrimPrefix(strings.TrimSpace(f.Get("notifarr_color")), "#"),
	}, nil
}

func emailFromForm(f url.Values, existing *notify.EmailConfig) (*notify.EmailConfig, error) {
	port, err := strconv.Atoi(strings.TrimSpace(f.Get("email_port")))
	if err != nil {
		return nil, errors.New("email: port must be a number")
	}
	password := f.Get("email_password")
	if password == "" && existing != nil {
		password = existing.Password
	}
	var to []string
	for _, addr := range strings.Split(f.Get("email_to"), ",") {
		if addr = strings.TrimSpace(addr); addr != "" {
			to = append(to, addr)
		}
	}
	if strings.TrimSpace(f.Get("email_host")) == "" || strings.TrimSpace(f.Get("email_from")) == "" || len(to) == 0 {
		return nil, errors.New("email: host, from address and at least one recipient are required")
	}
	return &notify.EmailConfig{
		Enabled: true, Host: strings.TrimSpace(f.Get("email_host")), Port: port,
		ImplicitTLS: f.Get("email_implicit_tls") != "", Username: strings.TrimSpace(f.Get("email_username")),
		Password: password, From: strings.TrimSpace(f.Get("email_from")), To: to,
	}, nil
}

func (u *ui) testNotifications(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cfg, err := u.c.NotifyConfig(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p := notificationsPage{base: u.newBase("Notifications", "notifications", r), Cfg: cfg, Form: formFromConfig(cfg)}
	if !cfg.Enabled() {
		p.TestError = "No channel is enabled yet."
		u.render(w, http.StatusOK, "notifications", p)
		return
	}
	p.TestResults = notify.Send(ctx, cfg, notify.Event{
		Job: "test", Kind: "test", Status: "failed",
		Summary: "This is a test notification from voidgrid-backup.",
	})
	u.render(w, http.StatusOK, "notifications", p)
}
