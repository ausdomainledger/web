package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/cors"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/tsenart/tb"
	"golang.org/x/crypto/acme/autocert"
)

var (
	throttler        *tb.Throttler
	db               *sqlx.DB
	throttleDisabled bool

	errThrottled = errors.New("Throttled")

	etldCount   int64
	domainCount int64
)

type queryResponse struct {
	Results []queryResult `json:"results"`
}

type queryResult struct {
	Domain    string `json:"domain" db:"domain"`
	ETLD      string `json:"etld" db:"etld"`
	FirstSeen int64  `json:"first_seen" db:"first_seen"`
	LastSeen  int64  `json:"last_seen" db:"last_seen"`
}

type statsResponse struct {
	DomainCount int64 `json:"domains"`
	ETLDCount   int64 `json:"etlds"`
}

func main() {
	var err error
	db, err = sqlx.Open("postgres", os.Getenv("LEDGER_WEB_DSN"))
	if err != nil {
		log.Fatal(err)
	}

	throttleDisabled = os.Getenv("LEDGER_WEB_NOTHROTTLE") != ""
	if !throttleDisabled {
		throttler = tb.NewThrottler(time.Second)
		defer throttler.Close()
	}

	r := chi.NewRouter()

	xo := cors.New(cors.Options{
		AllowedOrigins: []string{os.Getenv("LEDGER_WEB_CORSORIGIN")},
		AllowedMethods: []string{"GET", "OPTIONS"},
		AllowedHeaders: []string{"Content-Type", "Accept"},
		MaxAge:         300,
	})
	r.Use(middleware.CloseNotify)
	r.Use(middleware.Timeout(10 * time.Second))
	r.Use(xo.Handler)
	r.Use(middleware.DefaultCompress)
	r.Use(middleware.Recoverer)

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/stats", statsHandler)

		r.Group(func(r chi.Router) {
			r.Use(checkLimit)
			r.Get("/query", queryHandler)
		})
	})

	go pollEtldCount()

	if ssl := os.Getenv("LEDGER_WEB_SSL"); ssl != "" {
		log.Fatal(http.Serve(autocert.NewListener(ssl), r))
	}

	if err = http.ListenAndServe(os.Getenv("LEDGER_WEB_LISTEN"), r); err != nil {
		log.Fatal(err)
	}
}

func queryHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	q := r.URL.Query()

	limit, _ := strconv.Atoi(q.Get("limit"))
	off, _ := strconv.Atoi(q.Get("offset"))

	res, err := query(ctx, q.Get("query"), off, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func query(ctx context.Context, qs string, offsetLastSeen int, limit int) (queryResponse, error) {
	if limit == 0 || limit > 1000 {
		limit = 1000
	}

	if len(qs) > 255 {
		return queryResponse{}, errors.New("Query too long")
	}
	if len(qs) < 3 {
		return queryResponse{}, errors.New("Query must be at least 3 characters")
	}

	var out []queryResult

	var err error
	if offsetLastSeen > 0 {
		// This will present overlapping results from the previous page
		// but I don't want to add another sort key so
		// they will have to be deduplicated at the frontend
		err = db.SelectContext(ctx, &out, "SELECT * FROM domains WHERE domain LIKE $1 AND last_seen <= $2 ORDER BY last_seen DESC LIMIT $3;", qs, offsetLastSeen, limit)
	} else {
		err = db.SelectContext(ctx, &out, "SELECT * FROM domains WHERE domain LIKE $1 ORDER BY last_seen DESC LIMIT $2;", qs, limit)
	}

	if err != nil {
		log.Printf("Query error: %v", err)
		return queryResponse{}, errors.New("Query failed :(")
	}

	return queryResponse{Results: out}, nil
}

func checkLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if throttleDisabled {
			next.ServeHTTP(w, r)
			return
		}
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if throttler.Halt(ip, 1, 5) {
			http.Error(w, "Throttled", 429)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func pollEtldCount() {
	for {
		if err := db.Get(&etldCount, "SELECT COUNT(DISTINCT etld) FROM domains"); err != nil {
			log.Printf("Failed to get etld count: %v", err)
		}

		if err := db.Get(&domainCount, "SELECT COUNT(*) FROM domains"); err != nil {
			log.Printf("Failed to get domain count: %v", err)
		}
		time.Sleep(time.Minute)
	}
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(statsResponse{
		DomainCount: domainCount,
		ETLDCount:   etldCount,
	})
}
