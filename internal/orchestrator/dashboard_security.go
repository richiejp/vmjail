package orchestrator

import "net/http"

// withDashboardSecurityHeaders constrains the optional browser surface. The
// general dashboard still has legacy inline templates; the terminal frame
// overrides CSP with a stricter script-src 'self' policy and no forms/images.
func withDashboardSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := w.Header()
		headers.Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'none'; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		headers.Set("Cross-Origin-Opener-Policy", "same-origin")
		headers.Set("Cross-Origin-Resource-Policy", "same-origin")
		headers.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		headers.Set("Referrer-Policy", "no-referrer")
		headers.Set("X-Content-Type-Options", "nosniff")
		headers.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}
