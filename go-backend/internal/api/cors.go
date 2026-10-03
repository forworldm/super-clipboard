package api

import (
	"net/http"
	"strings"
)

// corsAllowMethods is Starlette's ALL_METHODS list used when
// CORSMiddleware(allow_methods=["*"]) is configured. The order is part of the
// wire format the original FastAPI app emits on preflight responses.
const corsAllowMethods = "DELETE, GET, HEAD, OPTIONS, PATCH, POST, PUT"

// corsMiddleware reproduces
//
//	app.add_middleware(CORSMiddleware, allow_origins=["*"], allow_methods=["*"], allow_headers=["*"])
//
// Requests without an Origin header pass through untouched. Preflight OPTIONS
// requests (those carrying Access-Control-Request-Method) are answered here
// and never reach the router. Every other cross-origin response gets the
// wildcard Allow-Origin header.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			writeCORSPreflight(w, r)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}

// writeCORSPreflight answers a browser preflight the way Starlette does:
// 200 "OK" (text/plain) with Allow-Origin, Allow-Methods, Allow-Headers and
// Max-Age: 600. Allow-Headers echoes Access-Control-Request-Headers when the
// browser sent one, otherwise "*". An unknown requested method yields 400
// "Disallowed CORS method", matching Starlette's failure path.
func writeCORSPreflight(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Access-Control-Allow-Origin", "*")
	header.Set("Access-Control-Allow-Methods", corsAllowMethods)
	header.Set("Access-Control-Max-Age", "600")
	if requestedHeaders := r.Header.Get("Access-Control-Request-Headers"); requestedHeaders != "" {
		header.Set("Access-Control-Allow-Headers", requestedHeaders)
	} else {
		header.Set("Access-Control-Allow-Headers", "*")
	}
	header.Set("Content-Type", "text/plain; charset=utf-8")

	requestedMethod := strings.ToUpper(strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")))
	if !corsMethodAllowed(requestedMethod) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("Disallowed CORS method"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func corsMethodAllowed(method string) bool {
	switch method {
	case http.MethodDelete, http.MethodGet, http.MethodHead,
		http.MethodOptions, http.MethodPatch, http.MethodPost, http.MethodPut:
		return true
	default:
		return false
	}
}

// statusRecorder captures the status code for the access log middleware and
// forwards optional interfaces (Flusher, etc.) via Unwrap so http.ResponseController
// and http.ServeContent keep working.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (recorder *statusRecorder) WriteHeader(status int) {
	if !recorder.wrote {
		recorder.status = status
		recorder.wrote = true
	}
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *statusRecorder) Write(body []byte) (int, error) {
	if !recorder.wrote {
		recorder.status = http.StatusOK
		recorder.wrote = true
	}
	return recorder.ResponseWriter.Write(body)
}

// Unwrap exposes the underlying ResponseWriter so http.NewResponseController
// can find Flusher / Hijacker / Deadliner implementations.
func (recorder *statusRecorder) Unwrap() http.ResponseWriter {
	return recorder.ResponseWriter
}

// Flush implements http.Flusher when the inner writer does.
func (recorder *statusRecorder) Flush() {
	if flusher, ok := recorder.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
