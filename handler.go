package sse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luraproject/lura/config"
	"github.com/luraproject/lura/logging"
	"github.com/luraproject/lura/proxy"
	router "github.com/luraproject/lura/router/gin"
)

// Config holds the configuration for SSE endpoints
type Config struct {
	KeepAliveInterval time.Duration `json:"keep_alive_interval"`
	RetryInterval     int           `json:"retry_interval"`
}

// HandlerFactory creates handlers for SSE endpoints
type HandlerFactory struct {
	logger logging.Logger
}

// Define custom context key type for Gin v1.7.7 compatibility
type contextKey string

const ginContextKey contextKey = "gin-context"

// NewHandlerFactory returns a new SSE HandlerFactory
func NewHandlerFactory(logger logging.Logger) *HandlerFactory {
	return &HandlerFactory{
		logger: logger,
	}
}

// HandlerWrapper wraps the standard handler factory to support SSE endpoints
func (s *HandlerFactory) HandlerWrapper(standardHandlerFactory router.HandlerFactory) router.HandlerFactory {
	return func(cfg *config.EndpointConfig, p proxy.Proxy) gin.HandlerFunc {
		s.logger.Debug(fmt.Sprintf("[ENDPOINT: %s] Building the http handler", cfg.Endpoint))

		// Check if this is an SSE endpoint
		if _, ok := cfg.ExtraConfig["sse"]; ok {
			// For SSE endpoints, we need to handle the request differently
			return func(c *gin.Context) {
				// Read and store the body FIRST, before any middleware processes it
				var bodyBytes []byte
				if c.Request.Body != nil {
					var err error
					bodyBytes, err = io.ReadAll(c.Request.Body)
					if err != nil {
						s.logger.Error("Error reading request body:", err)
						c.JSON(http.StatusBadRequest, gin.H{"error": "Error reading request body"})
						return
					}
					// Restore the body for downstream handlers
					c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
				}

				// Store the raw body for the SSE handler
				c.Set("rawBody", bodyBytes)

				// Add Gin context to the request context for middleware compatibility
				ctx := context.WithValue(c.Request.Context(), ginContextKey, c)
				c.Request = c.Request.WithContext(ctx)

				// Create middleware chain for auth/validation/metrics but with a noop endpoint
				validateHandler := standardHandlerFactory(cfg, func(ctx context.Context, _ *proxy.Request) (*proxy.Response, error) {
					// Just return nil to signal that processing should continue
					// The body is already stored in the Gin context
					return nil, nil
				})

				// Run middleware chain for validation/auth/etc.
				validateHandler(c)

				// If the middleware aborted the request, don't continue
				if c.IsAborted() {
					return
				}

				// Now run the SSE handler
				sseHandler := s.NewHandler(cfg, p)
				sseHandler(c)
			}
		}

		// Return standard handler for non-SSE endpoints
		return standardHandlerFactory(cfg, p)
	}
}

// NewHandler creates a new SSE handler
func (s *HandlerFactory) NewHandler(cfg *config.EndpointConfig, _ proxy.Proxy) gin.HandlerFunc {
	return func(c *gin.Context) {
		// First, make the backend request to determine response type
		s.processBackendRequest(c, cfg)
	}
}

// setupSSEConnection sets up the SSE headers and initial configuration
func (s *HandlerFactory) setupSSEConnection(c *gin.Context, cfg *config.EndpointConfig) Config {
	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// Make sure a 200 status is set early
	c.Status(http.StatusOK)

	// Get SSE config
	var sseCfg Config
	if v, ok := cfg.ExtraConfig["sse"]; ok && v != nil {
		if b, err := json.Marshal(v); err == nil {
			json.Unmarshal(b, &sseCfg)
		}
	}

	// Set default values
	if sseCfg.KeepAliveInterval == 0 {
		sseCfg.KeepAliveInterval = 30 * time.Second
	}
	if sseCfg.RetryInterval == 0 {
		sseCfg.RetryInterval = 1000
	}

	// Don't send retry interval here - let the backend handle initial messages
	// The retry interval will be sent as part of the streamed response if needed

	return sseCfg
}

// startKeepAlive starts the keepalive goroutine
func (s *HandlerFactory) startKeepAlive(c *gin.Context, sseCfg Config) (context.Context, context.CancelFunc) {
	keepAliveCtx, keepAliveCancel := context.WithCancel(context.Background())

	go func() {
		ticker := time.NewTicker(sseCfg.KeepAliveInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				c.Writer.WriteString(": keepalive\n\n")
				c.Writer.Flush()
			case <-keepAliveCtx.Done():
				return
			}
		}
	}()

	return keepAliveCtx, keepAliveCancel
}

// processBackendRequest validates the backend configuration and processes the request
func (s *HandlerFactory) processBackendRequest(c *gin.Context, cfg *config.EndpointConfig) {
	// Validate backend configuration
	if len(cfg.Backend) == 0 {
		s.logger.Error("No backend configured for SSE endpoint")
		c.Writer.WriteString("event: error\ndata: {\"message\":\"No backend configured\"}\n\n")
		c.Writer.Flush()
		return
	}

	backendConfig := cfg.Backend[0]
	if len(backendConfig.Host) == 0 {
		s.logger.Error("No host configured for SSE backend")
		c.Writer.WriteString("event: error\ndata: {\"message\":\"No host configured\"}\n\n")
		c.Writer.Flush()
		return
	}

	// Continue with request processing
	s.prepareAndExecuteRequest(c, cfg, *backendConfig)
}

// prepareAndExecuteRequest prepares and executes the backend request
func (s *HandlerFactory) prepareAndExecuteRequest(c *gin.Context, cfg *config.EndpointConfig, backendConfig config.Backend) {
	// Construct the backend URL
	backendURL := fmt.Sprintf("%s%s", backendConfig.Host[0], backendConfig.URLPattern)
	s.logger.Debug(fmt.Sprintf("SSE backend URL: %s", backendURL))

	// Get request body
	bodyBytes, ok := s.getRequestBody(c)
	if !ok {
		return
	}

	// Create and send request
	req, err := s.createRequest(c, backendConfig, backendURL, bodyBytes)
	if err != nil {
		return
	}

	// Execute request and process response
	s.executeRequestAndHandleResponse(c, req, cfg)
}

// getRequestBody extracts the request body from the context
func (s *HandlerFactory) getRequestBody(c *gin.Context) ([]byte, bool) {
	rawBody, exists := c.Get("rawBody")
	if !exists {
		s.logger.Error("Request body not found in context")
		c.Writer.WriteString("event: error\ndata: {\"message\":\"Request body not found\"}\n\n")
		c.Writer.Flush()
		return nil, false
	}
	return rawBody.([]byte), true
}

// createRequest creates a new HTTP request
func (s *HandlerFactory) createRequest(c *gin.Context, backendConfig config.Backend,
	backendURL string, bodyBytes []byte) (*http.Request, error) {

	req, err := http.NewRequestWithContext(c.Request.Context(),
		backendConfig.Method,
		backendURL,
		bytes.NewReader(bodyBytes))

	if err != nil {
		s.logger.Error("Error creating backend request:", err)
		fmt.Fprintf(c.Writer, "event: error\ndata: {\"message\":\"Error creating request: %s\"}\n\n", err)
		c.Writer.Flush()
		return nil, err
	}

	// Copy relevant headers
	for k, v := range c.Request.Header {
		req.Header[k] = v
	}

	return req, nil
}

// executeRequestAndHandleResponse executes the request and handles the response
func (s *HandlerFactory) executeRequestAndHandleResponse(c *gin.Context, req *http.Request, cfg *config.EndpointConfig) {
	// Create a new HTTP client
	client := &http.Client{
		Timeout: 0, // No timeout for streaming connections
	}

	// Make the request
	resp, err := client.Do(req)
	if err != nil {
		s.logger.Error("Error making backend request:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Error making request"})
		return
	}
	defer resp.Body.Close()

	// Determine response type based on Content-Type header
	contentType := resp.Header.Get("Content-Type")
	isSSEResponse := (contentType == "text/event-stream" || contentType == "text/plain")

	s.logger.Debug(fmt.Sprintf("Backend response Content-Type: %s, treating as SSE: %v", contentType, isSSEResponse))

	if isSSEResponse {
		// Set up SSE connection and stream the response
		s.setupSSEConnectionAndStream(c, cfg, resp)
	} else {
		// Handle as regular JSON response
		s.handleJSONResponse(c, resp)
	}
}

// setupSSEConnectionAndStream sets up SSE connection and streams the response
func (s *HandlerFactory) setupSSEConnectionAndStream(c *gin.Context, cfg *config.EndpointConfig, resp *http.Response) {
	// Set up the SSE connection
	sseCfg := s.setupSSEConnection(c, cfg)

	// Check response status first
	if resp.StatusCode != http.StatusOK {
		s.logger.Warning(fmt.Sprintf("Backend returned non-200 status: %d", resp.StatusCode))
		fmt.Fprintf(c.Writer, "event: error\ndata: {\"message\":\"Backend returned status %d\"}\n\n", resp.StatusCode)
		c.Writer.Flush()
		return
	}

	// Start keep-alive mechanism only after we start streaming
	_, keepAliveCancel := s.startKeepAlive(c, sseCfg)
	defer keepAliveCancel()

	// Stream the response directly without any initial messages
	s.streamResponse(c, resp)
}

// handleJSONResponse handles regular JSON responses
func (s *HandlerFactory) handleJSONResponse(c *gin.Context, resp *http.Response) {
	// Copy response headers (except content-length which will be recalculated)
	for k, v := range resp.Header {
		if k != "Content-Length" {
			c.Header(k, v[0])
		}
	}

	// Set the status code
	c.Status(resp.StatusCode)

	// Copy the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		s.logger.Error("Error reading response body:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Error reading response"})
		return
	}

	// Write the body directly to maintain original formatting
	c.Writer.Write(body)
}

// streamResponse streams the response to the client
func (s *HandlerFactory) streamResponse(c *gin.Context, resp *http.Response) {
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				s.logger.Error("SSE read error:", err)
			}
			break
		}

		// Write the line directly to the client
		c.Writer.Write(line)
		c.Writer.Flush()
	}
}

// New creates a new SSE middleware that wraps the provided handler factory
func New(handlerFactory router.HandlerFactory, logger logging.Logger) router.HandlerFactory {
	sseFactory := NewHandlerFactory(logger)
	return sseFactory.HandlerWrapper(handlerFactory)
}
