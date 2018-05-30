package router

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zimwip/hello/config"
	"github.com/zimwip/hello/middleware"

	"github.com/gorilla/mux"
	"github.com/opentracing/opentracing-go"
	oplog "github.com/opentracing/opentracing-go/log"
	"github.com/unrolled/secure"
	"gopkg.in/olahol/melody.v1"

	"go.uber.org/zap"
)

type APIRouter struct {
	http.Server
	router *mux.Router
	logger *zap.Logger
}

// GopherInfo contains information about the gopher on screen
type GopherInfo struct {
	ID, X, Y string
}

func handler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var parentCtx opentracing.SpanContext
	parentSpan := opentracing.SpanFromContext(r.Context())
	if parentSpan != nil {
		parentCtx = parentSpan.Context()
	}

	sp := opentracing.StartSpan("handler", opentracing.ChildOf(parentCtx)) // Start a new root span.
	defer sp.Finish()
	ctx = opentracing.ContextWithSpan(ctx, sp)

	vars := mux.Vars(r)
	w.WriteHeader(http.StatusOK)
	sp.LogFields(
		oplog.String("event", "soft error"),
		oplog.String("type", "cache timeout"),
		oplog.Int("waited.millis", 1500))
	csp := opentracing.StartSpan("Event 1", opentracing.ChildOf(sp.Context()))
	defer csp.Finish()
	csp.LogFields(oplog.String("test", "test"))
	fmt.Fprintf(w, "Category: %v\n", vars["category"])
	w.Write([]byte("Gorilla!\n"))
}

func panicHandler(w http.ResponseWriter, r *http.Request) {
	sp := opentracing.StartSpan("GET /panic") // Start a new root span.
	defer sp.Finish()
	panic("Oh no !")
}

func HealthCheckHandler(w http.ResponseWriter, r *http.Request) {
	// A very simple health check.
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")

	// In the future we could report back on the status of our DB, or our cache
	// (e.g. Redis) by performing a simple PING, and include them in the response.
	w.Write([]byte(`{"alive": true}`))
}

func PrintUsage(r *APIRouter) {
	err := r.router.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		pathTemplate, err := route.GetPathTemplate()
		if err == nil {
			fmt.Println("ROUTE:", pathTemplate)
		}
		pathRegexp, err := route.GetPathRegexp()
		if err == nil {
			fmt.Println("Path regexp:", pathRegexp)
		}
		queriesTemplates, err := route.GetQueriesTemplates()
		if err == nil {
			fmt.Println("Queries templates:", strings.Join(queriesTemplates, ","))
		}
		queriesRegexps, err := route.GetQueriesRegexp()
		if err == nil {
			fmt.Println("Queries regexps:", strings.Join(queriesRegexps, ","))
		}
		methods, err := route.GetMethods()
		if err == nil {
			fmt.Println("Methods:", strings.Join(methods, ","))
		}
		fmt.Println()
		return nil
	})

	if err != nil {
		fmt.Println(err)
	}
}

/*
Create a new server listen to API call and static file
return an APIRouter
*/
func NewServer(secured_port string, port string, staticDir string, logger *zap.Logger) *APIRouter {

	// master router
	r := mux.NewRouter()
	// Set up classic Negroni Middleware
	recovery := middleware.NewRecovery()
	recovery.Formatter = &middleware.HTMLPanicFormatter{}
	recovery.PrintStack = true

	r.Use(middleware.NewLogger(logger).Middleware)
	r.Use(recovery.Middleware)

	secureMiddleware := secure.New(secure.Options{
		HostsProxyHeaders:    []string{"X-Forwarded-Host"},
		SSLRedirect:          true,
		SSLHost:              secured_port,
		SSLProxyHeaders:      map[string]string{"X-Forwarded-Proto": "https"},
		STSSeconds:           315360000,
		STSIncludeSubdomains: true,
		STSPreload:           true,
		FrameDeny:            true,
		ContentTypeNosniff:   true,
		BrowserXssFilter:     true,
		//		ContentSecurityPolicy: "script-src $NONCE",
		PublicKey:     `pin-sha256="base64+primary=="; pin-sha256="base64+backup=="; max-age=5184000; includeSubdomains; report-uri="https://www.example.com/hpkp-report"`,
		IsDevelopment: config.Config().IsDev(),
	})

	r.Use(secureMiddleware.Handler)

	// api route setup
	api := r.PathPrefix("/api").Subrouter().StrictSlash(true)
	api.HandleFunc("/", handler)
	api.HandleFunc("/articles/{category}", handler).Methods("GET")
	api.HandleFunc("/panic", panicHandler)
	api.HandleFunc("/health", HealthCheckHandler)

	// Now websocket test
	mrouter := melody.New()
	gophers := make(map[*melody.Session]*GopherInfo)
	lock := new(sync.Mutex)
	counter := 0

	r.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		mrouter.HandleRequest(w, r)
	})

	mrouter.HandleConnect(func(s *melody.Session) {
		lock.Lock()
		for _, info := range gophers {
			s.Write([]byte("set " + info.ID + " " + info.X + " " + info.Y))
		}
		gophers[s] = &GopherInfo{strconv.Itoa(counter), "0", "0"}
		s.Write([]byte("iam " + gophers[s].ID))
		counter++
		lock.Unlock()
	})

	mrouter.HandleDisconnect(func(s *melody.Session) {
		lock.Lock()
		mrouter.BroadcastOthers([]byte("dis "+gophers[s].ID), s)
		delete(gophers, s)
		lock.Unlock()
	})

	mrouter.HandleMessage(func(s *melody.Session, msg []byte) {
		p := strings.Split(string(msg), " ")
		lock.Lock()
		info := gophers[s]
		if len(p) == 2 {
			info.X = p[0]
			info.Y = p[1]
			mrouter.BroadcastOthers([]byte("set "+info.ID+" "+info.X+" "+info.Y), s)
		}
		lock.Unlock()
	})

	// static route
	static := r.PathPrefix("/").Subrouter().StrictSlash(true)
	static.HandleFunc("/", handler)
	static.Use(middleware.NewStatic(http.Dir(staticDir)).Middleware)

	// get TLSConfig
	tlsConfig, manager := GetTLSConfig()
	// create the server,
	srv := &APIRouter{http.Server{
		Addr:      secured_port,
		TLSConfig: tlsConfig,
		// Good practice to set timeouts to avoid Slowloris attacks.
		WriteTimeout: time.Second * 15,
		ReadTimeout:  time.Second * 15,
		IdleTimeout:  time.Second * 60,
		Handler:      r, // Pass our instance of gorilla/mux in.
	}, r, logger}

	// Run our server in a goroutine so that it doesn't block.
	go func() {
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			logger.Fatal("Server stop with error", zap.Error(err))
		}
	}()

	// allow ACME call to be performed
	go func() {
		if !config.Config().IsDev() && manager != nil {
			if err := http.ListenAndServe(port, manager.HTTPHandler(nil)); err != nil {
				logger.Fatal("Server stop with error", zap.Error(err))
			}
		}
	}()

	return srv
}
