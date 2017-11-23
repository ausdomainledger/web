// This program is just a web server that serves
// queries between PG and the visitor, based on the data
// collected by scanner. Nothing interesting.
// No open source licence is provided for this code.

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
	"strings"
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
	Last    uint64        `json:"last"`
}

type queryResult struct {
	Domain    string `json:"domain" db:"domain"`
	ETLD      string `json:"etld" db:"etld"`
	FirstSeen int64  `json:"first_seen" db:"first_seen"`
	LastSeen  int64  `json:"last_seen" db:"last_seen"`
	Id        uint64 `json:"id" db:"id"`
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
	off, _ := strconv.Atoi(q.Get("from_time"))
	last, _ := strconv.Atoi(q.Get("last_id"))

	res, err := query(ctx, q.Get("query"), off, last, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func query(ctx context.Context, qs string, fromTime int, lastId int, limit int) (queryResponse, error) {
	if limit == 0 || limit > 1000 {
		limit = 1000
	}

	if len(qs) > 255 {
		return queryResponse{}, errors.New("Query too long")
	}
	if len(qs) < 3 {
		return queryResponse{}, errors.New("Query must be at least 3 characters")
	}

	qs = strings.ToLower(strings.TrimSpace(qs))

	var out []queryResult

	var err error
	if fromTime > 0 && lastId > 0 {
		err = db.SelectContext(ctx, &out, "SELECT * FROM domains WHERE domain LIKE $1 AND first_seen <= $2 AND id < $4 ORDER BY first_seen DESC, last_seen DESC, id DESC LIMIT $3;", qs, fromTime, limit, lastId)
	} else if lastId == 0 && fromTime > 0 {
		err = db.SelectContext(ctx, &out, "SELECT * FROM domains WHERE domain LIKE $1 AND first_seen <= $2 ORDER BY first_seen DESC, last_seen DESC, id DESC LIMIT $3;", qs, fromTime, limit)
	} else {
		err = db.SelectContext(ctx, &out, "SELECT * FROM domains WHERE domain LIKE $1 ORDER BY first_seen DESC, last_seen DESC, id DESC LIMIT $2;", qs, limit)
	}

	if err != nil {
		log.Printf("Query error: %v", err)
		return queryResponse{}, errors.New("Query failed :(")
	}

	lowestId := ^uint64(0)
	for _, v := range out {
		if v.Id < lowestId {
			lowestId = v.Id
		}
	}

	return queryResponse{Results: out, Last: lowestId}, nil
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
		if err := db.Get(&etldCount, "SELECT COUNT(*) FROM (SELECT DISTINCT etld FROM domains) AS temp;"); err != nil {
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
