package admin

import (
	_ "embed"
	"net/http"
	"strings"
)

//go:embed dashboard.html
var dashboardHTML string

//go:embed login.html
var loginHTML string

// AppVersion is the single source of truth for the dashboard version badge.
// Bump this on every release so the sidebar/footer always show the current version.
const AppVersion = "v1.5.11"

// ServeDashboard serves the admin dashboard. Injects the warning box on the login screen
// and warning banner inside the dashboard only if the password is still the default "123456"
// or "change-me-to-a-strong-secret".
func (a *AdminHandler) ServeDashboard(w http.ResponseWriter, r *http.Request) {
	secret := a.getAdminSecret()

	var passBox string
	var banner string

	if secret == "123456" || secret == "change-me-to-a-strong-secret" || secret == "1111qqqq" {
		passBox = `
    <div style="background:var(--red-soft);border:1px solid var(--red-border);border-radius:var(--radius);padding:.6rem .85rem;margin-bottom:1rem;font-size:.8rem;display:flex;align-items:center;gap:.6rem;color:var(--red)">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="flex-shrink:0"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg>
      <div><strong style="color:var(--red)">Security Warning:</strong> <span style="color:var(--text2)">Your admin secret is still default. Change it immediately!</span></div>
    </div>`

		banner = `
  <div style="background:var(--red-soft);border:1px solid var(--red-border);color:var(--red);border-radius:var(--radius);padding:.85rem 1.15rem;margin-bottom:1.25rem;display:flex;align-items:center;gap:.75rem;font-size:.85rem">
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="flex-shrink:0"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg>
    <div>
      <strong style="color:var(--red)">Security Notice:</strong> You are currently using a default administrator password. Please change it immediately using the <strong>Password</strong> button in the top navigation bar to secure your gateway.
    </div>
  </div>`
	}

	html := strings.ReplaceAll(dashboardHTML, "{{DEFAULT_PASS_BOX}}", passBox)
	html = strings.ReplaceAll(html, "{{DEFAULT_PASSWORD_BANNER}}", banner)
	html = strings.ReplaceAll(html, "{{APP_VERSION}}", AppVersion)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// ServeLogin serves the standalone admin login page.
func (a *AdminHandler) ServeLogin(w http.ResponseWriter, r *http.Request) {
	secret := a.getAdminSecret()

	var passBox string
	if secret == "123456" || secret == "change-me-to-a-strong-secret" || secret == "1111qqqq" {
		passBox = `
    <div style="background:var(--red-soft);border:1px solid var(--red-border);border-radius:var(--radius);padding:.6rem .85rem;margin-bottom:1rem;font-size:.8rem;display:flex;align-items:center;gap:.6rem;color:var(--red)">
      <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="flex-shrink:0"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg>
      <div><strong style="color:var(--red)">Security Warning:</strong> <span style="color:var(--text2)">Your admin secret is still default. Change it immediately after login!</span></div>
    </div>`
	}

	html := strings.ReplaceAll(loginHTML, "{{DEFAULT_PASS_BOX}}", passBox)
	html = strings.ReplaceAll(html, "{{APP_VERSION}}", AppVersion)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}
