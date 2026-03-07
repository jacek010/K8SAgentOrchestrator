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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/cache"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/controller"
)

// Server wraps the Gin HTTP engine with all orchestrator dependencies.
type Server struct {
	engine    *gin.Engine
	client    client.Client
	cache     *cache.AgentCacheManager
	reconciler *controller.AgentReconciler
	namespace string // default namespace if not provided in path
}

// NewServer creates and configures the REST server.
func NewServer(
	k8sClient client.Client,
	cache *cache.AgentCacheManager,
	reconciler *controller.AgentReconciler,
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

		// Agent CRUD  – scoped to namespace
		ns := v1.Group("/namespaces/:namespace")
		{
			agents := ns.Group("/agents")
			{
				agents.POST("", s.handleCreateAgent)
				agents.GET("", s.handleListAgents)
				agents.GET("/:name", s.handleGetAgent)
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
		}
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

// apiCtx returns a context with a 30-second API timeout.
func apiCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
