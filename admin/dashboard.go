package admin

import (
	_ "embed"
	"fmt"
	"net/http"
	"strings"
)

//go:embed dashboard.html
var dashboardHTML string

// ServeDashboard serves the admin dashboard. Injects the warning box on the login screen
// and warning banner inside the dashboard only if the password is still the default "123456"
// or "change-me-to-a-strong-secret".
func (a *AdminHandler) ServeDashboard(w http.ResponseWriter, r *http.Request) {
	secret := a.getAdminSecret()

	var passBox string
	var banner string

	if secret == "123456" || secret == "change-me-to-a-strong-secret" {
		passBox = fmt.Sprintf(`
    <div style="background:rgba(239,68,68,0.1);border:1px solid rgba(239,68,68,0.2);border-radius:var(--radius);padding:.6rem .8rem;margin-bottom:1rem;font-size:.8rem;display:flex;align-items:center;gap:.5rem">
      <span style="color:#ef4444;font-weight:600">⚠️ Default Password:</span>
      <code style="color:var(--cyan);font-weight:600;font-family:'Cascadia Code','Fira Code',monospace">%s</code>
    </div>`, secret)

		banner = fmt.Sprintf(`
  <div style="background:rgba(239,68,68,0.12);border:1px solid rgba(239,68,68,0.25);color:#f87171;border-radius:var(--radius);padding:1rem;margin-bottom:1.5rem;display:flex;align-items:center;gap:0.75rem;font-size:0.9rem">
    <span style="font-size:1.25rem">⚠️</span>
    <div>
      <strong style="color:#ef4444">Security Warning:</strong> You are currently using the default admin password <code style="background:rgba(0,0,0,0.2);padding:0.1rem 0.3rem;border-radius:3px;color:var(--cyan)">%s</code>. Please change it immediately by clicking the <strong>🔑 Change Password</strong> button in the top right corner to secure your gateway!
    </div>
  </div>`, secret)
	}

	html := strings.ReplaceAll(dashboardHTML, "{{DEFAULT_PASS_BOX}}", passBox)
	html = strings.ReplaceAll(html, "{{DEFAULT_PASSWORD_BANNER}}", banner)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}
