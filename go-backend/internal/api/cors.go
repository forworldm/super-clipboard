package api

import (
	"net/http"
)

// corsMiddleware reproduces
//
//	app.add_middleware(CORSMiddleware, allow_origins=["*"], allow_methods=["*"], allow_headers=["*"])
//
// Requests without an Origin header pass through untouched, preflight requests
// are answered directly and every other response carries the wildcard header.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			header := w.Header()
			header.Set("Access-Control-Allow-Origin", "*")
			header.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code for the access log middleware.
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
