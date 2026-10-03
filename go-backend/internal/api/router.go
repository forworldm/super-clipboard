package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
)

// Match kinds mirror starlette.routing.Match.
const (
	matchNone = iota
	matchPartial
	matchFull
)

type routeSegment struct {
	literal  string
	name     string
	wildcard bool
}

// route reproduces starlette.routing.APIRoute: a path made of literal and
// `{param}` segments plus the set of HTTP methods it accepts (GET routes also
// accept HEAD, exactly like Starlette).
type route struct {
	path     string
	segments []routeSegment
	methods  []string
	handler  http.HandlerFunc
}

func newRoute(path string, methods []string, handler http.HandlerFunc) *route {
	methodSet := make(map[string]bool, len(methods)+1)
	for _, method := range methods {
		methodSet[strings.ToUpper(method)] = true
	}
	if methodSet[http.MethodGet] {
		methodSet[http.MethodHead] = true
	}
	normalized := make([]string, 0, len(methodSet))
	for method := range methodSet {
		normalized = append(normalized, method)
	}
	sort.Strings(normalized)

	segments := make([]routeSegment, 0, 4)
	for _, chunk := range splitPath(path) {
		if strings.HasPrefix(chunk, "{") && strings.HasSuffix(chunk, "}") {
			segments = append(segments, routeSegment{name: chunk[1 : len(chunk)-1], wildcard: true})
			continue
		}
		segments = append(segments, routeSegment{literal: chunk})
	}
	return &route{path: path, segments: segments, methods: normalized, handler: handler}
}

// splitPath keeps Starlette semantics: only the leading slash is dropped, so a
// trailing slash produces an empty final segment ("/api/clips/" therefore does
// not match the "/api/clips" route and triggers redirect_slashes instead).
func splitPath(path string) []string {
	if path == "" {
		return nil
	}
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// match returns the match kind plus the captured path parameters.
func (rt *route) match(path string, method string) (int, map[string]string) {
	candidates := splitPath(path)
	if len(candidates) != len(rt.segments) {
		return matchNone, nil
	}
	params := make(map[string]string, len(rt.segments))
	for index, segment := range rt.segments {
		value := candidates[index]
		if segment.wildcard {
			if value == "" {
				return matchNone, nil
			}
			params[segment.name] = value
			continue
		}
		if segment.literal != value {
			return matchNone, nil
		}
	}
	if method != "" && !containsMethod(rt.methods, method) {
		return matchPartial, params
	}
	return matchFull, params
}

func containsMethod(methods []string, method string) bool {
	for _, candidate := range methods {
		if candidate == method {
			return true
		}
	}
	return false
}

// router reproduces starlette.routing.Router: routes are evaluated in
// registration order, the first full match wins, otherwise the first partial
// match yields a 405, otherwise `redirect_slashes` is attempted and finally a
// JSON 404 is returned.
type router struct {
	routes   []*route
	notFound http.HandlerFunc
}

func newRouter() *router {
	return &router{notFound: func(w http.ResponseWriter, r *http.Request) {
		writeError(w, newHTTPError(http.StatusNotFound, "Not Found"))
	}}
}

func (rt *router) add(path string, methods []string, handler http.HandlerFunc) {
	rt.routes = append(rt.routes, newRoute(path, methods, handler))
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := requestPath(r)
	var partial *route
	for _, candidate := range rt.routes {
		kind, params := candidate.match(path, r.Method)
		switch kind {
		case matchFull:
			candidate.handler.ServeHTTP(w, withRouteParams(r, params))
			return
		case matchPartial:
			if partial == nil {
				partial = candidate
			}
		}
	}
	if partial != nil {
		w.Header().Set("Allow", strings.Join(partial.methods, ", "))
		writeError(w, newHTTPError(http.StatusMethodNotAllowed, "Method Not Allowed"))
		return
	}

	// redirect_slashes
	if path != "/" {
		alternate := path + "/"
		if strings.HasSuffix(path, "/") {
			alternate = strings.TrimSuffix(path, "/")
			if alternate == "" {
				alternate = "/"
			}
		}
		if rt.matchesAnyPath(alternate) {
			target := alternate
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
			return
		}
	}
	rt.notFound(w, r)
}

func (rt *router) matchesAnyPath(path string) bool {
	for _, candidate := range rt.routes {
		if kind, _ := candidate.match(path, ""); kind != matchNone {
			return true
		}
	}
	return false
}

func requestPath(r *http.Request) string {
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	return path
}

type paramsKey struct{}

func withRouteParams(r *http.Request, params map[string]string) *http.Request {
	if len(params) == 0 {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), paramsKey{}, params))
}

// routeParam returns a captured `{name}` path segment.
func routeParam(r *http.Request, name string) string {
	value, ok := r.Context().Value(paramsKey{}).(map[string]string)
	if !ok {
		return ""
	}
	return value[name]
}

// queryValue reads a raw query parameter and reports whether it was present.
func queryValue(r *http.Request, name string) (string, bool) {
	values, ok := r.URL.Query()[name]
	if !ok || len(values) == 0 {
		return "", false
	}
	return values[0], true
}
