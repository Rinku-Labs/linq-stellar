package api

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// The test console is served on the test network, each file with the CSP
// that lets only its own script run.
func TestTheConsoleIsServedOnTheTestNetwork(t *testing.T) {
	p := newTestnetPublicAPI(t)
	for path, contentType := range map[string]string{
		"/console/":        "text/html",
		"/console/app.js":  "text/javascript",
		"/console/app.css": "text/css",
	} {
		w := p.send(http.MethodGet, path, "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, w.Code)
		}
		if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, contentType) {
			t.Errorf("%s: content type %q", path, got)
		}
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "default-src 'none'") ||
			strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP %q", path, csp)
		}
		for header, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := w.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q, want %q", path, header, got, want)
			}
		}
	}
	if w := p.send(http.MethodGet, "/console", "", nil); w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/console/" {
		t.Errorf("/console: %d to %q", w.Code, w.Header().Get("Location"))
	}
}

// On mainnet a key would move real money, and the console keeps one in a
// browser tab: there is no console there.
func TestThereIsNoConsoleOnMainnet(t *testing.T) {
	p := newPublicAPI(t)
	for _, path := range []string{"/console/", "/console/app.js"} {
		if w := p.send(http.MethodGet, path, "", nil); w.Code == http.StatusOK {
			t.Errorf("%s served on mainnet", path)
		}
	}
}

// The page's safety rests on two things the CSP can't check by itself: no
// inline script or handler in the HTML, and no API value ever parsed as HTML
// by the script. Every value the API returns is set with textContent.
func TestTheConsoleNeverTreatsDataAsHTML(t *testing.T) {
	html, err := consoleFiles.ReadFile("console/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]*\S[^<]*</script>|<script>`).Match(html) {
		t.Error("index.html has an inline script")
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).Match(html) {
		t.Error("index.html has an inline event handler")
	}
	js, err := consoleFiles.ReadFile("console/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(string(js), banned) {
			t.Errorf("app.js uses %s", banned)
		}
	}
}

// The console is a client of /v1 and nothing else: it can do nothing the key
// couldn't do with curl. Every path it calls starts /v1/.
func TestTheConsoleCallsOnlyV1(t *testing.T) {
	js, err := consoleFiles.ReadFile("console/app.js")
	if err != nil {
		t.Fatal(err)
	}
	calls := regexp.MustCompile(`api\("(?:GET|POST|PUT|DELETE)",\s*"([^"]*)"`).FindAllStringSubmatch(string(js), -1)
	if len(calls) == 0 {
		t.Fatal("found no API calls; the pattern is out of date")
	}
	for _, c := range calls {
		if !strings.HasPrefix(c[1], "/v1/") {
			t.Errorf("app.js calls %s", c[1])
		}
	}
	if strings.Contains(string(js), "/admin") {
		t.Error("app.js mentions /admin")
	}
}
