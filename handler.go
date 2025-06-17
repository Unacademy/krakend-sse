package sse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luraproject/lura/v2/config"
	"github.com/luraproject/lura/v2/logging"
	"github.com/luraproject/lura/v2/proxy"
	router "github.com/luraproject/lura/v2/router/gin"
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
			// Create middleware chain for auth/validation/metrics but with a noop endpoint
			// This applies all middleware but doesn't actually process the request
			validateHandler := standardHandlerFactory(cfg, func(ctx context.Context, _ *proxy.Request) (*proxy.Response, error) {
				// Store the raw body in the context for the SSE handler to use
				if c, ok := ctx.Value(gin.ContextKey).(*gin.Context); ok {
					// Get the raw body from the request
					if body, err := io.ReadAll(c.Request.Body); err == nil {
						// Restore the body for downstream handlers
						c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
						// Store the raw body for the SSE handler
						c.Set("rawBody", body)
					}
				}
				// Return nil to signal that processing should continue
				return nil, nil
			})

			// Create combined handler that runs middleware chain first, then SSE handler
			return func(c *gin.Context) {
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
		// Set up the SSE connection
		sseCfg := s.setupSSEConnection(c, cfg)

		// Start keep-alive mechanism
		_, keepAliveCancel := s.startKeepAlive(c, sseCfg)
		defer keepAliveCancel()

		// Validate backend configuration and proceed with request if valid
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

	// Send retry interval
	c.Writer.WriteString("retry: " + strconv.Itoa(sseCfg.RetryInterval) + "\n\n")
	c.Writer.Flush()

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
	s.prepareAndExecuteRequest(c, *backendConfig)
}

// prepareAndExecuteRequest prepares and executes the backend request
func (s *HandlerFactory) prepareAndExecuteRequest(c *gin.Context, backendConfig config.Backend) {
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
	s.executeRequestAndHandleResponse(c, req)
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
func (s *HandlerFactory) executeRequestAndHandleResponse(c *gin.Context, req *http.Request) {
	// Create a new HTTP client
	client := &http.Client{
		Timeout: 0, // No timeout for streaming connections
	}

	// Make the request
	resp, err := client.Do(req)
	if err != nil {
		s.logger.Error("Error making backend request:", err)
		fmt.Fprintf(c.Writer, "event: warning\ndata: {\"message\":\"Error making request: %s\"}\n\n", err)
		c.Writer.Flush()
		return
	}
	defer resp.Body.Close()

	// Check response status
	if resp.StatusCode != http.StatusOK {
		s.logger.Warning(fmt.Sprintf("Backend returned non-200 status: %d", resp.StatusCode))
		fmt.Fprintf(c.Writer, "event: warning\ndata: {\"message\":\"Backend returned status %d\"}\n\n", resp.StatusCode)
		c.Writer.Flush()
	}

	// Process the response stream
	s.streamResponse(c, resp)
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
