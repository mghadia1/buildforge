package remote

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/mghadia1/buildforge/internal/cache"
	"github.com/mghadia1/buildforge/internal/digest"
)

// Server exposes a local store over HTTP.
//
//	GET  /cas/<digest>   fetch a blob
//	PUT  /cas/<digest>   upload a blob
//	GET  /ac/<key>       fetch an action cache entry
//	PUT  /ac/<key>       upload an action cache entry
//
// Concurrent writers need no coordination: the store writes to a temporary file
// and renames it into place, and two clients uploading the same blob are by
// definition uploading the same bytes. That is not a lucky property of this
// implementation — it is what content addressing buys. A key-value cache where
// the value is not determined by the key would need locking here.
//
// There is no authentication and no transport security. This is a cache for a
// trusted network, and it says so rather than implying otherwise.
type Server struct {
	store *cache.Store

	// Logger receives one line per rejected request. Nil is silent.
	Logger *log.Logger
}

// NewServer wraps a store in an HTTP handler.
func NewServer(store *cache.Store) *Server { return &Server{store: store} }

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/cas/", s.handleCAS)
	mux.HandleFunc("/ac/", s.handleAC)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	return mux
}

func (s *Server) handleCAS(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/cas/")

	switch r.Method {
	case http.MethodGet:
		// The store validates the name before it becomes a path component, so a
		// digest of "../../etc/passwd" is refused rather than served.
		b, err := s.store.ReadBlob(digest.Digest(name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(b)

	case http.MethodPut:
		b, ok := s.readBody(w, r)
		if !ok {
			return
		}
		// WriteBlob recomputes the digest. A client that uploads bytes under the
		// wrong address is refused, because everything downstream trusts that a
		// blob's name describes its contents.
		if err := s.store.WriteBlob(digest.Digest(name), b); err != nil {
			s.reject(w, r, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(http.StatusCreated)

	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAC(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/ac/")

	switch r.Method {
	case http.MethodGet:
		entry, err := s.store.Lookup(key)
		if err != nil || entry == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entry)

	case http.MethodPut:
		b, ok := s.readBody(w, r)
		if !ok {
			return
		}
		var e cache.Entry
		if err := json.Unmarshal(b, &e); err != nil {
			s.reject(w, r, http.StatusBadRequest, err)
			return
		}
		// An entry whose key does not match the URL it was uploaded to would let
		// a client overwrite an unrelated action's record.
		if e.Key != key {
			s.reject(w, r, http.StatusBadRequest, errKeyMismatch)
			return
		}
		if err := s.store.PutEntry(&e); err != nil {
			s.reject(w, r, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(http.StatusCreated)

	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	// A cache accepts uploads from other machines, so an unbounded read is a way
	// for any one of them to exhaust this server's memory.
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBlobBytes+1))
	if err != nil {
		s.reject(w, r, http.StatusBadRequest, err)
		return nil, false
	}
	if len(b) > maxBlobBytes {
		s.reject(w, r, http.StatusRequestEntityTooLarge, errTooLarge)
		return nil, false
	}
	return b, true
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request, code int, err error) {
	if s.Logger != nil {
		s.Logger.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	}
	http.Error(w, http.StatusText(code), code)
}

type constError string

func (e constError) Error() string { return string(e) }

const (
	errKeyMismatch = constError("entry key does not match the request path")
	errTooLarge    = constError("payload too large")
)
