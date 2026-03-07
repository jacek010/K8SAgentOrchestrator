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
)

// Server wraps the Gin HTTP engine with all orchestrator dependencies.
type Server struct {
	engine     *gin.Engine
	client     client.Client
	cache      *cache.AgentCacheManager
	reconciler *controller.AgentReconciler
	recorder   record.EventRecorder
	namespace  string // default namespace if not provided in path
}

// NewServer creates and configures the REST server.
func NewServer(
	k8sClient client.Client,
	cache *cache.AgentCacheManager,
	reconciler *controller.AgentReconciler,
	recorder record.EventRecorder,
	defaultNamespace string,
	debug bool,
) *Server {
	if !debug {
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(loggerMiddleware())

	s := &Server{
		engine:     engine,
		client:     k8sClient,
		cache:      cache,
		reconciler: reconciler,
		recorder:   recorder,
		namespace:  defaultNamespace,
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
	agents.GET("/:name", s.handleGetAgent)
	agents.GET("/:name/history", s.handleGetAgentHistory)
	agents.PUT("/:name", s.handleUpdateAgent)
	agents.DELETE("/:name", s.handleDeleteAgent)

	// Lifecycle operations
	agents.POST("/:name/restart", s.handleRestartAgent)
	agents.POST("/:name/stop", s.handleStopAgent)
	agents.POST("/:name/start", s.handleStartAgent)
	agents.POST("/:name/disable-healing", s.handleDisableSelfHealing)
	agents.POST("/:name/enable-healing", s.handleEnableSelfHealing)

	// Environment variable management
	agents.GET("/:name/env", s.handleGetEnv)
	agents.PUT("/:name/env", s.handleSetEnv)
	agents.PATCH("/:name/env", s.handleMergeEnv)
	agents.DELETE("/:name/env/:key", s.handleDeleteEnvKey)

	// Log streaming
	agents.GET("/:name/logs", s.handleGetLogs)

	// Per-agent in-memory cache
	agents.GET("/:name/cache", s.handleListCache)
	agents.GET("/:name/cache/:field", s.handleGetCacheField)
	agents.PUT("/:name/cache/:field", s.handleSetCacheField)
	agents.DELETE("/:name/cache/:field", s.handleDeleteCacheField)
	agents.DELETE("/:name/cache", s.handleClearCache)
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

// apiCtx returns a context with a 30-second API timeout.
func apiCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
