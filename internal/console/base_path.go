package console

import "net/http"

// href turns a console-absolute path ("/console/...") into the URL the browser
// must request when notify is mounted under Host.BasePath. notify strips the
// prefix before routing, so route patterns and Go-built paths stay unprefixed;
// every URL the console emits (links, form actions, Datastar targets,
// redirects, cookie Paths, assets) goes through here at the point of emission.
// Empty strings (no link) and non-rooted values ("#", absolute URLs) pass
// through, and with an empty prefix the result is always p unchanged.
func (c *Console) href(p string) string {
	if p == "" || p[0] != '/' {
		return p
	}
	return c.deps.BasePath + p
}

// redirect is http.Redirect for console-local targets.
func (c *Console) redirect(w http.ResponseWriter, r *http.Request, target string, code int) {
	http.Redirect(w, r, c.href(target), code)
}
