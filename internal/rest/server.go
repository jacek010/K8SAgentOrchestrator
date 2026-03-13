// Package rest implements the HTTP REST API server for the orchestrator.
package rest

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/cache"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/controller"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/history"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/idle"
)

// Server wraps the Gin HTTP engine with all orchestrator dependencies.
type Server struct {
	engine      *gin.Engine
	client      client.Client
	cache       cache.CacheStore
	history     history.HistoryStore
	reconciler  *controller.AgentReconciler
	recorder    record.EventRecorder
	namespace   string        // default namespace if not provided in path
	idleWatcher *idle.Watcher // nil means idle tracking disabled
}

// NewServer creates and configures the REST server.
func NewServer(
	k8sClient client.Client,
	cache cache.CacheStore,
	reconciler *controller.AgentReconciler,
	recorder record.EventRecorder,
	defaultNamespace string,
	debug bool,
	idleWatcher *idle.Watcher,
	historyStore history.HistoryStore,
) *Server {
	if !debug {
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(corsMiddleware())
	engine.Use(loggerMiddleware())

	s := &Server{
		engine:      engine,
		client:      k8sClient,
		cache:       cache,
		history:     historyStore,
		reconciler:  reconciler,
		recorder:    recorder,
		namespace:   defaultNamespace,
		idleWatcher: idleWatcher,
	}

	s.registerRoutes()
	return s
}

// Run starts the HTTP server on the given address.
func (s *Server) Run(addr string) error {
	return s.engine.Run(addr)
}

// Handler returns the underlying http.Handler (useful for testing).
func (s *Server) Handler() http.Handler {
	return s.engine
}

// registerRoutes wires all API endpoints.
func (s *Server) registerRoutes() {
	// Swagger UI — http://localhost:8082/swagger/index.html
	s.engine.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	v1 := s.engine.Group("/api/v1")
	{
		// Info (namespace, version)
		v1.GET("/info", s.handleInfo)

		// Health & readiness
		s.engine.GET("/healthz", s.handleHealthz)
		s.engine.GET("/readyz", s.handleReadyz)

		// Namespaced path: /api/v1/namespaces/:namespace/agents/...
		// Explicit namespace in the URL takes precedence.
		nsGroup := v1.Group("/namespaces/:namespace")
		s.registerAgentRoutes(nsGroup.Group("/agents"))

		// Short path: /api/v1/agents/...
		// Namespace param is absent so nsName() falls back to s.namespace (default).
		s.registerAgentRoutes(v1.Group("/agents"))
	}
}

// registerAgentRoutes attaches all agent endpoints to the given RouterGroup.
// The group may or may not contain a :namespace param in its base path;
// nsName() handles both cases transparently.
func (s *Server) registerAgentRoutes(agents *gin.RouterGroup) {
	agents.POST("", s.handleCreateAgent)
	agents.GET("", s.handleListAgents)
	agents.GET("/services", s.handleListAgentServices)

	// All /:name/* routes share an activity-tracking middleware that resets the
	// idle timer whenever any named-agent endpoint is called.
	named := agents.Group("/:name", s.activityMiddleware())
	named.GET("", s.handleGetAgent)
	named.GET("/history", s.handleGetAgentHistory)
	named.PUT("", s.handleUpdateAgent)
	named.DELETE("", s.handleDeleteAgent)

	// Lifecycle operations
	named.POST("/restart", s.handleRestartAgent)
	named.POST("/stop", s.handleStopAgent)
	named.POST("/start", s.handleStartAgent)
	named.POST("/disable-healing", s.handleDisableSelfHealing)
	named.POST("/enable-healing", s.handleEnableSelfHealing)

	// Idle keep-alive: resets idle timer, wakes agent if paused, waits until Running.
	named.POST("/keepalive", s.handleKeepalive)

	// Environment variable management
	named.GET("/env", s.handleGetEnv)
	named.PUT("/env", s.handleSetEnv)
	named.PATCH("/env", s.handleMergeEnv)
	named.DELETE("/env/:key", s.handleDeleteEnvKey)

	// Log streaming
	named.GET("/logs", s.handleGetLogs)

	// Per-agent in-memory cache
	named.GET("/cache", s.handleListCache)
	named.GET("/cache/:field", s.handleGetCacheField)
	named.PUT("/cache/:field", s.handleSetCacheField)
	named.DELETE("/cache/:field", s.handleDeleteCacheField)
	named.DELETE("/cache", s.handleClearCache)
}

// corsMiddleware adds CORS headers so browser-based clients (e.g. the web UI
// served on a different port) can call the REST API.
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		c.Header("Access-Control-Max-Age", "86400")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// loggerMiddleware is a minimal structured logger.
func loggerMiddleware() gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(p gin.LogFormatterParams) string {
		return fmt.Sprintf("[REST] %v | %d | %s | %s %s\n",
			p.TimeStamp.Format("2006/01/02 15:04:05"),
			p.StatusCode,
			p.Latency,
			p.Method,
			p.Path,
		)
	})
}

// activityMiddleware records the current time in the agent cache so the idle
// watcher knows when this agent was last active. It is a no-op when idleWatcher is nil.
func (s *Server) activityMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.idleWatcher != nil {
			ns := c.Param("namespace")
			if ns == "" {
				ns = s.namespace
			}
			name := c.Param("name")
			if name != "" {
				s.idleWatcher.TouchActivity(ns, name)
			}
		}
		c.Next()
	}
}

// apiCtx returns a context with a 30-second API timeout.
func apiCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
