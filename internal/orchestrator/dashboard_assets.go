package orchestrator

import (
	"embed"
	"net/http"

	"github.com/richiejp/darkaifactory/internal/assert"
)

// dashboardAssets contains reviewed, release-pinned browser code. The xterm
// files retain their upstream license under web/thirdparty/xterm/LICENSE.
//
//go:embed web/terminal.js web/thirdparty/xterm/xterm.js web/thirdparty/xterm/xterm.css web/thirdparty/xterm/LICENSE
var dashboardAssets embed.FS

func registerDashboardAssetRoutes(mux *http.ServeMux) {
	serve := func(route, file, contentType string) {
		data, err := dashboardAssets.ReadFile(file)
		assert.NoErr(err, "read embedded dashboard asset %s", file)
		mux.HandleFunc("GET "+route, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
			_, _ = w.Write(data)
		})
	}
	serve("/assets/terminal.js", "web/terminal.js", "text/javascript; charset=utf-8")
	serve("/assets/xterm.js", "web/thirdparty/xterm/xterm.js", "text/javascript; charset=utf-8")
	serve("/assets/xterm.css", "web/thirdparty/xterm/xterm.css", "text/css; charset=utf-8")
}
